package cron

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testJob(name string, everyMS int64) Job {
	return Job{
		Name: name,
		Schedule: Schedule{Kind: KindEvery, EveryMS: &everyMS},
		Payload: Payload{
			Kind: PayloadAgentTurn, Message: "x",
			SessionKey: "webui:test", OriginChannel: "webui", OriginChatID: "test",
		},
	}
}

func TestSchedulerLeaseRejectsSecondGateway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	first := NewService(path, nil)
	second := NewService(path, nil)
	if err := first.Load(); err != nil { t.Fatal(err) }
	if err := second.Load(); err != nil { t.Fatal(err) }
	if err := first.Start(); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = first.Close(ctx)
	})
	if err := second.Start(); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second Start error=%v want ErrLeaseHeld", err)
	}
}

func TestSchedulerTimeoutAndCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	started := make(chan struct{}, 1)
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return RunResult{RunID: runID}, ctx.Err()
	})
	if err := s.Load(); err != nil { t.Fatal(err) }
	job := testJob("cancel", 60_000)
	job.TimeoutMS = 25
	added, err := s.AddJob(job)
	if err != nil { t.Fatal(err) }
	if err := s.RunNow(added.ID, true); err != nil { t.Fatal(err) }
	<-started
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, _ := s.GetJob(added.ID)
		if len(got.State.RunHistory) == 1 {
			if got.State.LastStatus != StatusError {
				t.Fatalf("status=%q", got.State.LastStatus)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout run did not finish")
}

func TestSchedulerConcurrencyLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	var running atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) { break }
		}
		defer running.Add(-1)
		select {
		case <-release:
			return RunResult{RunID: runID}, nil
		case <-ctx.Done():
			return RunResult{RunID: runID}, ctx.Err()
		}
	})
	s.SetMaxConcurrent(2)
	if err := s.Load(); err != nil { t.Fatal(err) }
	var jobs []Job
	for i := 0; i < 3; i++ {
		j, err := s.AddJob(testJob("j", 60_000))
		if err != nil { t.Fatal(err) }
		jobs = append(jobs, j)
	}
	if err := s.RunNow(jobs[0].ID, true); err != nil { t.Fatal(err) }
	if err := s.RunNow(jobs[1].ID, true); err != nil { t.Fatal(err) }
	if err := s.RunNow(jobs[2].ID, true); err == nil {
		t.Fatal("third run unexpectedly bypassed concurrency limit")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.ActiveCount() == 0 { break }
		time.Sleep(5 * time.Millisecond)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency=%d", peak.Load())
	}
}

func TestScheduledIdempotencyKeyUsesScheduleInstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	var mu sync.Mutex
	var got Job
	done := make(chan struct{}, 1)
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) {
		mu.Lock(); got = job; mu.Unlock()
		done <- struct{}{}
		return RunResult{RunID: runID}, nil
	})
	if err := s.Load(); err != nil { t.Fatal(err) }
	j, err := s.AddJob(testJob("idem", 60_000))
	if err != nil { t.Fatal(err) }
	if err := s.RunNow(j.ID, true); err != nil { t.Fatal(err) }
	<-done
	mu.Lock(); snapshot := got; mu.Unlock()
	if snapshot.ScheduledForMS == 0 {
		t.Fatal("missing scheduled instant")
	}
	want := idempotencyKey(j.ID, snapshot.ScheduledForMS)
	if snapshot.IdempotencyKey != want {
		t.Fatalf("idempotency=%q want %q", snapshot.IdempotencyKey, want)
	}
}

func TestMisfirePolicySkipAndFireOnce(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour).UnixMilli()
	every := int64(time.Minute / time.Millisecond)

	fire := testJob("fire", every)
	fire.State.NextRunAtMS = &past
	fire.MisfirePolicy = MisfireFireOnce
	if !shouldFireMisfire(fire, now) { t.Fatal("fire_once should execute one missed run") }

	skip := fire
	skip.MisfirePolicy = MisfireSkip
	if shouldFireMisfire(skip, now) { t.Fatal("skip policy should not execute missed run") }

	grace := fire
	grace.MisfireGraceMS = int64((10 * time.Minute) / time.Millisecond)
	if shouldFireMisfire(grace, now) { t.Fatal("expired grace should skip missed run") }
}

func TestDSTRepeatedWallClockDoesNotDoubleFire(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil { t.Skip(err) }
	// 2026-11-01 01:30 occurs twice. Starting after the first occurrence must
	// not schedule the same local wall-clock key again.
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	s := Schedule{Kind: KindCron, Expr: "30 1 * * *", TZ: "America/New_York"}
	next, err := nextRecurringAfter(s, first.Add(time.Minute), first.UnixMilli())
	if err != nil { t.Fatal(err) }
	if next == nil { t.Fatal("missing next run") }
	if cronWallKey(s, time.UnixMilli(*next)) == cronWallKey(s, first) {
		t.Fatalf("repeated DST wall-clock scheduled twice: %s", time.UnixMilli(*next).In(loc))
	}
}
