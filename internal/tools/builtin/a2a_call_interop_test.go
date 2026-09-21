package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestA2AInteropAgainstALiveAgent exercises the outbound client against a real
// peer rather than a stub, which is the only way to catch the class of defect
// that a hand-written fake cannot: a dual-stack name whose first resolved
// address refuses the connection, or a peer whose card advertises an interface
// URL that is not "<base>/a2a".
//
// It is opt-in because it needs a running A2A agent. Point it at anything
// reachable from this host:
//
//	A2A_INTEROP_URL=http://127.0.0.1:9999 go test ./internal/tools/builtin/ -run TestA2AInterop -v
func TestA2AInteropAgainstALiveAgent(t *testing.T) {
	baseURL := os.Getenv("A2A_INTEROP_URL")
	if baseURL == "" {
		t.Skip("A2A_INTEROP_URL is not set; skipping live A2A interop")
	}

	// Loopback is blocked by default, so the test has to allowlist it explicitly.
	tool := NewA2ACall([]string{"127.0.0.1", "localhost"})
	ctx := context.Background()

	discover, _ := json.Marshal(map[string]any{"action": "discover", "agent_url": baseURL})
	res, err := tool.Execute(ctx, discover)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if strings.Contains(res.Content, "blocked by outbound policy") || strings.Contains(res.Content, "discovery failed") {
		t.Fatalf("discovery did not reach the peer:\n%s", res.Content)
	}
	t.Logf("discover:\n%s", res.Content)

	send, _ := json.Marshal(map[string]any{"action": "send", "agent_url": baseURL, "task_message": "ping from haosbot"})
	res, err = tool.Execute(ctx, send)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(res.Content, "A2A Response from") {
		t.Fatalf("the peer did not answer with a task or message:\n%s", res.Content)
	}
	t.Logf("send:\n%s", res.Content)
}
