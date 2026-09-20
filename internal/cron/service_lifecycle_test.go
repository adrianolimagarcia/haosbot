package cron

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newLifecycleService(t *testing.T, calls *atomic.Int32) (*Service, Job) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	s := NewService(path, func(_ context.Context, _ Job, runID string) (RunResult, error) {
		calls.Add(1)
		return RunResult{RunID: runID}, nil
	})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	ms := int64(60_000)
	job, err := s.AddJob(Job{
		Name: "lifecycle", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms},
		Payload: Payload{Kind: PayloadAgentTurn, Message: "x", SessionKey: "webui:a", OriginChannel: "webui", OriginChatID: "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, job
}

// RunNow used to be accepted after Close returned, launching a job against a
// canceled context and adding to the WaitGroup while Close was already waiting
// on it (which panics with "WaitGroup is reused before previous Wait has
// returned").
func TestRunNowAfterCloseIsRejected(t *testing.T) {
	var calls atomic.Int32
	s, job := newLifecycleService(t, &calls)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.RunNow(job.ID, true); !errors.Is(err, ErrClosed) {
		t.Fatalf("RunNow after Close = %v, want ErrClosed", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("executor ran %d times after shutdown, want 0", n)
	}
	if s.Running() {
		t.Fatal("Running() is true after Close")
	}
}

// The scheduler must stay usable across a Close/Start cycle.
func TestStartClearsClosedState(t *testing.T) {
	var calls atomic.Int32
	s, job := newLifecycleService(t, &calls)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Close(c)
	})
	if err := s.RunNow(job.ID, true); err != nil {
		t.Fatalf("RunNow after restart: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("job did not run after a restart")
	}
}

// A rollback of a failed mutation must restore the previous dirty flag instead
// of clearing it: dirty is what blocks a second side-effecting run until the
// state that was already pending persistence actually lands.
func TestRollbackPreservesPendingDirtyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	s := NewService(path, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	ms := int64(60_000)
	job, err := s.AddJob(Job{
		Name: "original", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms},
		Payload: Payload{Kind: PayloadAgentTurn, Message: "x", SessionKey: "webui:a", OriginChannel: "webui", OriginChatID: "a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()

	// Force the save to fail deterministically: a temp file cannot replace an
	// existing directory.
	s.storePath = t.TempDir()

	renamed := "mutated"
	if _, err := s.UpdateJob(job.ID, Update{Name: &renamed}); err == nil {
		t.Fatal("UpdateJob unexpectedly succeeded with a directory store target")
	}
	s.mu.Lock()
	dirty := s.dirty
	s.mu.Unlock()
	if !dirty {
		t.Fatal("UpdateJob rollback cleared a pre-existing dirty state")
	}

	if err := s.RemoveJob(job.ID); err == nil {
		t.Fatal("RemoveJob unexpectedly succeeded with a directory store target")
	}
	s.mu.Lock()
	dirty = s.dirty
	s.mu.Unlock()
	if !dirty {
		t.Fatal("RemoveJob rollback cleared a pre-existing dirty state")
	}
}
