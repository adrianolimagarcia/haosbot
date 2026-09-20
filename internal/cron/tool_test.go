package cron

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

func TestCronToolBindsAuthoritativeRequestRoute(t *testing.T) {
	s := NewService(filepath.Join(t.TempDir(), "cron", "jobs.json"), nil)
	if err := s.Load(); err != nil { t.Fatal(err) }
	if err := s.Start(); err != nil { t.Fatal(err) }
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Close(ctx)
	}()

	tool := NewTool(s, "UTC")
	ctx := tools.WithRequestRoute(context.Background(), tools.RequestRoute{
		SessionKey: "unified:override",
		Channel: "webui",
		ChatID: "real-chat-id",
		Metadata: map[string]any{"source": "webui", "nested": map[string]any{"x": 1}},
	})
	raw := json.RawMessage(`{"action":"add","name":"route-test","message":"work","every_seconds":3600}`)
	result, err := tool.Execute(ctx, raw)
	if err != nil { t.Fatal(err) }
	if result.IsError { t.Fatalf("tool returned error: %s", result.Content) }

	jobs := s.ListJobs(true)
	if len(jobs) != 1 { t.Fatalf("jobs=%d want 1", len(jobs)) }
	got := jobs[0].Payload
	if got.SessionKey != "unified:override" || got.OriginChannel != "webui" || got.OriginChatID != "real-chat-id" {
		t.Fatalf("route=%#v", got)
	}
	if got.OriginMetadata["source"] != "webui" {
		t.Fatalf("metadata=%#v", got.OriginMetadata)
	}
}

func TestCronToolRejectsMutationWithoutGatewayScheduler(t *testing.T) {
	s := NewService(filepath.Join(t.TempDir(), "cron", "jobs.json"), nil)
	if err := s.Load(); err != nil { t.Fatal(err) }
	tool := NewTool(s, "UTC")
	ctx := tools.WithRequestRoute(context.Background(), tools.RequestRoute{
		SessionKey: "webui:x", Channel: "webui", ChatID: "x",
	})
	result, err := tool.Execute(ctx, json.RawMessage(`{"action":"add","message":"x","every_seconds":60}`))
	if err != nil { t.Fatal(err) }
	if !result.IsError {
		t.Fatalf("expected scheduler-not-running error, got %q", result.Content)
	}
	if len(s.ListJobs(true)) != 0 {
		t.Fatal("tool mutated jobs while scheduler was not running")
	}
}
