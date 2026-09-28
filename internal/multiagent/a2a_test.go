package multiagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestA2AExecutorExtractsArtifactText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"task-x","result":{"task":{"status":{"state":"TASK_STATE_COMPLETED"},"artifacts":[{"artifactId":"a","parts":[{"text":"remote result"}]}]}}}`))
	}))
	defer server.Close()

	exec := A2AExecutor{Client: server.Client(), Token: func(Profile) string { return "secret" }}
	result, err := exec.Execute(context.Background(), Profile{ID: "remote", Endpoint: server.URL}, Task{
		ID: "task-x", RootTaskID: "task-x", TraceID: "trace-x", Prompt: "hello",
	})
	if err != nil { t.Fatal(err) }
	if strings.TrimSpace(result) != "remote result" {
		t.Fatalf("result = %q", result)
	}
}
