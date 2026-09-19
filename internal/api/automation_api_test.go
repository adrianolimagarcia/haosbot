package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	cronruntime "github.com/adrianolimagarcia/nanobot-go/internal/cron"
)

func automationTestServer(t *testing.T) (*Server, *cronruntime.Service) {
	t.Helper()
	cfg := config.DefaultConfig()
	service := cronruntime.NewService(filepath.Join(t.TempDir(), "cron", "jobs.json"),
		func(ctx context.Context, job cronruntime.Job, runID string) (cronruntime.RunResult, error) {
			return cronruntime.RunResult{RunID: runID, Response: "ok"}, nil
		})
	if err := service.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, nil, nil)
	s.SetScheduler(service)
	return s, service
}

func TestWebUIAutomationCRUD(t *testing.T) {
	s, _ := automationTestServer(t)
	every := int64(60_000)
	body, _ := json.Marshal(map[string]any{
		"name": "daily",
		"message": "do work",
		"session_key": "webui:test",
		"schedule": map[string]any{"kind": "every", "everyMs": every},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webui/automations", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleWebUIAutomations(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	var created cronruntime.Job
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Payload.SessionKey != "webui:test" {
		t.Fatalf("created=%#v", created)
	}

	patch, _ := json.Marshal(map[string]any{"enabled": false, "name": "paused"})
	req = httptest.NewRequest(http.MethodPatch, "/api/webui/automation?id="+created.ID, bytes.NewReader(patch))
	rr = httptest.NewRecorder()
	s.handleWebUIAutomation(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", rr.Code, rr.Body.String())
	}
	var updated cronruntime.Job
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil { t.Fatal(err) }
	if updated.Enabled || updated.Name != "paused" {
		t.Fatalf("updated=%#v", updated)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/webui/automation?id="+created.ID, nil)
	rr = httptest.NewRecorder()
	s.handleWebUIAutomation(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWebUIAutomationRunNow(t *testing.T) {
	s, service := automationTestServer(t)
	ms := int64(60_000)
	job, err := service.AddJob(cronruntime.Job{
		Name: "run", Schedule: cronruntime.Schedule{Kind: cronruntime.KindEvery, EveryMS: &ms},
		Payload: cronruntime.Payload{Kind: cronruntime.PayloadAgentTurn, Message: "go", SessionKey: "webui:x", OriginChannel: "webui", OriginChatID: "x"},
	})
	if err != nil { t.Fatal(err) }

	req := httptest.NewRequest(http.MethodPost, "/api/webui/automation/run?id="+job.ID, nil)
	rr := httptest.NewRecorder()
	s.handleWebUIAutomationRun(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("run status=%d body=%s", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := service.GetJob(job.ID)
		if len(got.State.RunHistory) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for automation run")
}


func TestWebUIAutomationPendingIsTransientButVisible(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	cfg := config.DefaultConfig()
	service := cronruntime.NewService(filepath.Join(t.TempDir(), "cron", "jobs.json"),
		func(ctx context.Context, job cronruntime.Job, runID string) (cronruntime.RunResult, error) {
			started <- struct{}{}
			select {
			case <-block:
				return cronruntime.RunResult{RunID: runID, Response: "done"}, nil
			case <-ctx.Done():
				return cronruntime.RunResult{RunID: runID}, ctx.Err()
			}
		})
	if err := service.Load(); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Errorf("close automation service: %v", err)
		}
	})
	s := NewServer(cfg, nil, nil)
	s.SetScheduler(service)

	ms := int64(60_000)
	job, err := service.AddJob(cronruntime.Job{
		Name: "pending", Schedule: cronruntime.Schedule{Kind: cronruntime.KindEvery, EveryMS: &ms},
		Payload: cronruntime.Payload{Kind: cronruntime.PayloadAgentTurn, Message: "x", SessionKey: "webui:x", OriginChannel: "webui", OriginChatID: "x"},
	})
	if err != nil { t.Fatal(err) }
	if err := service.RunNow(job.ID, true); err != nil { t.Fatal(err) }
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/webui/automation?id="+job.ID, nil)
	rr := httptest.NewRecorder()
	s.handleWebUIAutomation(rr, req)
	if rr.Code != http.StatusOK { t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String()) }
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil { t.Fatal(err) }
	state, _ := payload["state"].(map[string]any)
	if pending, _ := state["pending"].(bool); !pending {
		t.Fatalf("pending not visible in API payload: %#v", payload)
	}

	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := service.GetJob(job.ID)
		if ok && !got.State.Pending && len(got.State.RunHistory) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for automation run cleanup")
}
