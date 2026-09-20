package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestServicePersistsAndReloadsJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	s := NewService(path, nil)
	if err := s.Load(); err != nil { t.Fatal(err) }
	ms := int64(60_000)
	added, err := s.AddJob(Job{Name: "test", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms}, Payload: Payload{Kind: PayloadAgentTurn, Message: "hello", SessionKey: "webui:abc", OriginChannel: "webui", OriginChatID: "abc"}})
	if err != nil { t.Fatal(err) }
	reloaded := NewService(path, nil)
	if err := reloaded.Load(); err != nil { t.Fatal(err) }
	jobs := reloaded.ListJobs(true)
	if len(jobs) != 1 || jobs[0].ID != added.ID || jobs[0].Payload.Message != "hello" { t.Fatalf("reloaded jobs=%#v", jobs) }
}

func TestServiceRunNowRecordsResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	var calls atomic.Int32
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) { calls.Add(1); return RunResult{RunID: runID, Response: "done"}, nil })
	if err := s.Load(); err != nil { t.Fatal(err) }
	ms := int64(60_000)
	job, err := s.AddJob(Job{Name: "run-now", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms}, Payload: Payload{Kind: PayloadAgentTurn, Message: "work", SessionKey: "webui:abc", OriginChannel: "webui", OriginChatID: "abc"}})
	if err != nil { t.Fatal(err) }
	if err := s.RunNow(job.ID, true); err != nil { t.Fatal(err) }
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := s.GetJob(job.ID)
		if ok && !got.State.Pending && len(got.State.RunHistory) == 1 {
			if got.State.LastStatus != StatusOK { t.Fatalf("last status=%q", got.State.LastStatus) }
			if calls.Load() != 1 { t.Fatalf("calls=%d want 1", calls.Load()) }
			runID := got.State.RunHistory[0].RunID
			record, err := s.ReadRunRecord(runID); if err != nil { t.Fatal(err) }
			if record["response"] != "done" { t.Fatalf("response=%#v", record["response"]) }
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for run")
}

func TestOneShotDeleteAfterRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) { return RunResult{RunID: runID}, nil })
	if err := s.Load(); err != nil { t.Fatal(err) }
	at := time.Now().Add(time.Hour).UnixMilli()
	job, err := s.AddJob(Job{Name: "once", Schedule: Schedule{Kind: KindAt, AtMS: &at}, DeleteAfterRun: true, Payload: Payload{Kind: PayloadAgentTurn, Message: "once", SessionKey: "webui:abc", OriginChannel: "webui", OriginChatID: "abc"}})
	if err != nil { t.Fatal(err) }
	if err := s.RunNow(job.ID, true); err != nil { t.Fatal(err) }
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) { if _, ok := s.GetJob(job.ID); !ok { return }; time.Sleep(10 * time.Millisecond) }
	t.Fatal("one-shot job was not removed")
}

func TestCorruptStoreIsPreserved(t *testing.T) {
	dir := t.TempDir(); path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil { t.Fatal(err) }
	s := NewService(path, nil)
	if err := s.Load(); err == nil { t.Fatal("expected corrupt store error") }
	matches, err := filepath.Glob(path + ".corrupt-*"); if err != nil { t.Fatal(err) }
	if len(matches) != 1 { t.Fatalf("corrupt backups=%v want exactly one", matches) }
	if _, err := os.Stat(path); !os.IsNotExist(err) { t.Fatalf("original corrupt path still present: %v", err) }
}

func TestRunErrorIsRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) { return RunResult{RunID: runID}, errors.New("boom") })
	if err := s.Load(); err != nil { t.Fatal(err) }
	ms := int64(60_000)
	job, err := s.AddJob(Job{Name: "fail", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms}, Payload: Payload{Kind: PayloadAgentTurn, Message: "x", SessionKey: "webui:a", OriginChannel: "webui", OriginChatID: "a"}})
	if err != nil { t.Fatal(err) }
	if err := s.RunNow(job.ID, true); err != nil { t.Fatal(err) }
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := s.GetJob(job.ID)
		if len(got.State.RunHistory) == 1 {
			if got.State.LastStatus != StatusError || got.State.LastError != "boom" { t.Fatalf("state=%#v", got.State) }
			raw, err := os.ReadFile(path); if err != nil { t.Fatal(err) }
			var stored Store; if err := json.Unmarshal(raw, &stored); err != nil { t.Fatal(err) }
			if stored.Jobs[0].State.LastStatus != StatusError { t.Fatalf("persisted state=%#v", stored.Jobs[0].State) }
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for failed run")
}

func TestMutationRollbackWhenStoreWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "jobs.json"); s := NewService(path, nil)
	if err := s.Load(); err != nil { t.Fatal(err) }
	ms := int64(60_000)
	job, err := s.AddJob(Job{Name: "original", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms}, Payload: Payload{Kind: PayloadAgentTurn, Message: "x", SessionKey: "webui:a", OriginChannel: "webui", OriginChatID: "a"}})
	if err != nil { t.Fatal(err) }
	badTarget := t.TempDir(); s.storePath = badTarget
	renamed := "mutated"
	if _, err := s.UpdateJob(job.ID, Update{Name: &renamed}); err == nil { t.Fatal("UpdateJob unexpectedly succeeded with directory store target") }
	got, ok := s.GetJob(job.ID); if !ok || got.Name != "original" { t.Fatalf("failed update mutated in-memory state: %#v", got) }
	if err := s.RemoveJob(job.ID); err == nil { t.Fatal("RemoveJob unexpectedly succeeded with directory store target") }
	got, ok = s.GetJob(job.ID); if !ok || got.Name != "original" { t.Fatalf("failed remove lost in-memory job: %#v", got) }
}

func TestDirtyExecutionBlocksAnotherRunUntilPersisted(t *testing.T) {
	root := t.TempDir(); path := filepath.Join(root, "cron", "jobs.json")
	var calls atomic.Int32
	s := NewService(path, func(ctx context.Context, job Job, runID string) (RunResult, error) { calls.Add(1); return RunResult{RunID: runID, Response: "side effect"}, nil })
	if err := s.Load(); err != nil { t.Fatal(err) }
	ms := int64(60_000)
	job, err := s.AddJob(Job{Name: "once-at-a-time", Schedule: Schedule{Kind: KindEvery, EveryMS: &ms}, Payload: Payload{Kind: PayloadAgentTurn, Message: "x", SessionKey: "webui:a", OriginChannel: "webui", OriginChatID: "a"}})
	if err != nil { t.Fatal(err) }
	s.storePath = t.TempDir()
	if err := s.RunNow(job.ID, true); err != nil { t.Fatal(err) }
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock(); dirty := s.dirty; active := s.active[job.ID]; s.mu.Unlock()
		if dirty && active == nil { break }
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock(); dirty := s.dirty; s.mu.Unlock()
	if !dirty { t.Fatal("expected dirty scheduler state after failed post-run save") }
	if err := s.RunNow(job.ID, true); err == nil { t.Fatal("second RunNow unexpectedly proceeded while dirty state was unpersisted") }
	if got := calls.Load(); got != 1 { t.Fatalf("executor calls=%d want 1", got) }
}
