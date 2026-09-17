package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
)

func TestMigrateLegacyGraphOutboxIsRestartSafe(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "graph-outbox.jsonl")
	data := "{\"op\":\"enqueue\",\"job_id\":\"old-1\",\"session_key\":\"s\",\"content\":\"hello\"}\n" +
		"{\"op\":\"enqueue\",\"job_id\":\"old-2\",\"session_key\":\"s\",\"content\":\"acked\"}\n" +
		"{\"op\":\"ack\",\"job_id\":\"old-2\"}\n"
	if err := os.WriteFile(legacy, []byte(data), 0o600); err != nil { t.Fatal(err) }
	fabric, err := memoryfabric.Open(context.Background(), memoryfabric.Config{Path: filepath.Join(dir, "memory-fabric.db"), MaxPending: 8})
	if err != nil { t.Fatal(err) }
	defer fabric.Close()
	if err := migrateLegacyGraphOutbox(legacy, fabric); err != nil { t.Fatal(err) }
	if err := migrateLegacyGraphOutbox(legacy, fabric); err != nil { t.Fatal(err) }
	job, ok, err := fabric.Claim(context.Background(), memoryfabric.ProjectionGraph)
	if err != nil || !ok { t.Fatalf("claim: ok=%v err=%v", ok, err) }
	if job.ID != "old-1" { t.Fatalf("job=%+v", job) }
	if _, ok, err := fabric.Claim(context.Background(), memoryfabric.ProjectionGraph); err != nil || ok { t.Fatalf("acked legacy record leaked: ok=%v err=%v", ok, err) }
}
