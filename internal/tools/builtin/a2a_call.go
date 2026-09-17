package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// A2ACallTool allows the nanobot agent to discover and delegate tasks to another A2A agent.
type A2ACallTool struct {
	params json.RawMessage
}

func NewA2ACall() *A2ACallTool {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"description": "Action to perform: 'discover' to read remote agent-card.json, or 'send' to delegate a task."
			},
			"agent_url": {
				"type": "string",
				"description": "The base URL or endpoint of the remote agent (e.g., 'http://100.76.224.27:8765' or 'http://remote-agent/a2a')."
			},
			"task_message": {
				"type": "string",
				"description": "The prompt, instruction or question to send to the remote agent (required when action is 'send')."
			}
		},
		"required": ["action", "agent_url"]
	}`)
	return &A2ACallTool{params: raw}
}

func (t *A2ACallTool) Name() string {
	return "a2a_call"
}

func (t *A2ACallTool) Description() string {
	return "Delegate a task to a remote AI agent using the official Agent2Agent (A2A) protocol. Can fetch agent card for discovery or send tasks via JSON-RPC."
}

func (t *A2ACallTool) Parameters() json.RawMessage {
	return t.params
}

func (t *A2ACallTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var input struct {
		Action      string `json:"action"`
		AgentURL    string `json:"agent_url"`
		TaskMessage string `json:"task_message"`
	}

	if err := json.Unmarshal(args, &input); err != nil {
		return tools.Result{Content: fmt.Sprintf("invalid arguments: %v", err)}, nil
	}

	baseURL := strings.TrimRight(strings.TrimSpace(input.AgentURL), "/")
	if baseURL == "" {
		return tools.Result{Content: "agent_url is required"}, nil
	}

	client := &http.Client{Timeout: 60 * time.Second}

	switch strings.ToLower(input.Action) {
	case "discover":
		discoveryURL := baseURL
		if !strings.HasSuffix(discoveryURL, "/.well-known/agent-card.json") {
			discoveryURL += "/.well-known/agent-card.json"
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
		if err != nil {
			return tools.Result{Content: err.Error()}, nil
		}

		resp, err := client.Do(req)
		if err != nil {
			return tools.Result{Content: fmt.Sprintf("discovery failed: %v", err)}, nil
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		return tools.Result{
			Content: fmt.Sprintf("Remote Agent Discovery:\n%s", string(body)),
		}, nil

	case "send":
		if strings.TrimSpace(input.TaskMessage) == "" {
			return tools.Result{Content: "task_message is required for send action"}, nil
		}

		endpoint := baseURL
		if !strings.HasSuffix(endpoint, "/a2a") {
			endpoint += "/a2a"
		}

		rpcReq := map[string]any{
			"jsonrpc": "2.0",
			"id":      time.Now().UnixNano(),
			"method":  "tasks/send",
			"params": map[string]any{
				"message": map[string]any{
					"text": input.TaskMessage,
				},
			},
		}
		payload, _ := json.Marshal(rpcReq)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return tools.Result{Content: err.Error()}, nil
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return tools.Result{Content: fmt.Sprintf("task delegation failed: %v", err)}, nil
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		return tools.Result{
			Content: fmt.Sprintf("A2A Response from %s:\n%s", endpoint, string(body)),
		}, nil

	default:
		return tools.Result{
			Content: fmt.Sprintf("unknown action: %s. Supported actions: 'discover', 'send'", input.Action),
		}, nil
	}
}
