package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGraphOutboxPersistsACKsAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph-outbox.jsonl")
	o, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	job := graphIndexJob{ID: "turn-fixed", sessionKey: "webui:s1", content: "hello"}
	if ok, err := o.Enqueue(job); err != nil || !ok {
		t.Fatalf("first enqueue: ok=%v err=%v", ok, err)
	}
	if ok, err := o.Enqueue(job); err != nil || !ok {
		t.Fatalf("duplicate enqueue: ok=%v err=%v", ok, err)
	}
	if got := len(o.Pending()); got != 1 {
		t.Fatalf("pending=%d, want 1 after duplicate", got)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.Pending(); len(got) != 1 || got[0].ID != job.ID {
		t.Fatalf("recovered pending=%+v", got)
	}
	if err := recovered.Ack(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	afterACK, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer afterACK.Close()
	if got := len(afterACK.Pending()); got != 0 {
		t.Fatalf("pending=%d after ACK/restart, want 0", got)
	}
}

func TestGraphIndexerDurableFullQueueRetryAndDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph-outbox.jsonl")
	o, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	g := newGraphIndexer(nil, 1, 1, o)
	var calls atomic.Int32
	started := make(chan string, 1)
	release := make(chan struct{})
	processed := make(chan string, 4)
	g.process = func(ctx context.Context, job graphIndexJob) error {
		calls.Add(1)
		select {
		case started <- job.ID:
		default:
		}
		if job.ID == "first" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		processed <- job.ID
		return nil
	}
	if !g.EnqueueWithID("first", "s", "one") {
		t.Fatal("first enqueue was rejected")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first job did not start")
	}
	if !g.EnqueueWithID("second", "s", "two") {
		t.Fatal("full queue should accept durably")
	}
	if !g.EnqueueWithID("second", "s", "two") {
		t.Fatal("duplicate durable enqueue should be idempotent")
	}
	if got := len(o.Pending()); got != 2 {
		t.Fatalf("pending=%d while first is blocked, want 2", got)
	}
	close(release)

	seen := map[string]int{}
	deadline := time.After(4 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-processed:
			seen[id]++
		case <-deadline:
			t.Fatalf("processed=%v calls=%d pending=%v", seen, calls.Load(), o.Pending())
		}
	}
	if seen["first"] != 1 || seen["second"] != 1 {
		t.Fatalf("processed=%v, want one execution per id", seen)
	}
	g.Close(context.Background())
	if got := len(o.Pending()); got != 0 {
		t.Fatalf("pending=%d after successful ACKs", got)
	}
}

func TestGraphOutboxCrashTailRecoveryAndIndexerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph-outbox.jsonl")
	o, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := o.Enqueue(graphIndexJob{ID: "crash-job", sessionKey: "s", content: "recover"}); err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("{\"version\":1,\"op\":\"enqueue\",\"job_id\":")
	_ = file.Close()

	recovered, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(recovered.Pending()); got != 1 {
		t.Fatalf("partial crash tail changed pending count to %d", got)
	}
	var processed atomic.Int32
	g := newGraphIndexer(nil, 1, 2, recovered)
	g.process = func(context.Context, graphIndexJob) error {
		processed.Add(1)
		return nil
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(recovered.Pending()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.Close(context.Background())
	defer recovered.Close()
	if processed.Load() == 0 {
		t.Fatal("recovered job was not processed after restart")
	}
	if len(recovered.Pending()) != 0 {
		t.Fatal("recovered job was not ACKed")
	}
}

func TestGraphOutboxRetryPersistsAttemptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph-outbox.jsonl")
	o, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	job := graphIndexJob{ID: "retry-job", sessionKey: "s", content: "retry"}
	if _, err := o.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	job.Attempts = 3
	if err := o.Retry(job, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openGraphOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	pending := recovered.Pending()
	if len(pending) != 1 || pending[0].Attempts != 3 {
		t.Fatalf("recovered retry state=%+v", pending)
	}
	if pending[0].NotBefore.Before(time.Now()) {
		t.Fatal("retry deadline was not persisted")
	}
}
