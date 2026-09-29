package multiagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type A2AExecutor struct {
	Client *http.Client
	Token  func(Profile) string
}

func (e A2AExecutor) Execute(ctx context.Context, profile Profile, task Task) (string, error) {
	if strings.TrimSpace(profile.Endpoint) == "" {
		return "", errors.New("multiagent: remote endpoint is empty")
	}
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	params := map[string]any{
		"message": map[string]any{
			"messageId": task.ID + "-message",
			"role":      "ROLE_USER",
			"parts":     []map[string]any{{"text": task.Prompt, "mediaType": "text/plain"}},
			"metadata": map[string]any{
				"haosTraceId": task.TraceID,
				"haosRootTaskId": task.RootTaskID,
				"haosParentTaskId": task.ParentTaskID,
				"haosDepth": task.Depth,
			},
		},
		"metadata": map[string]any{"haosRequestedBy": task.RequestedBy},
	}
	envelope := map[string]any{"jsonrpc": "2.0", "id": task.ID, "method": "message/send", "params": params}
	body, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(profile.Endpoint, "/"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Token != nil {
		if token := strings.TrimSpace(e.Token(profile)); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("multiagent: A2A request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("multiagent: A2A HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		return "", fmt.Errorf("multiagent: decode A2A response: %w", err)
	}
	if rpcErr, ok := wire["error"].(map[string]any); ok {
		return "", fmt.Errorf("multiagent: A2A error: %v", rpcErr["message"])
	}
	result, _ := wire["result"].(map[string]any)
	if message, ok := result["message"].(map[string]any); ok {
		if text := extractParts(message["parts"]); text != "" {
			return text, nil
		}
	}
	taskObj, _ := result["task"].(map[string]any)
	if taskObj == nil {
		return "", errors.New("multiagent: A2A response contains neither task nor message")
	}
	var chunks []string
	if artifacts, ok := taskObj["artifacts"].([]any); ok {
		for _, rawArtifact := range artifacts {
			if artifact, ok := rawArtifact.(map[string]any); ok {
				if text := extractParts(artifact["parts"]); text != "" {
					chunks = append(chunks, text)
				}
			}
		}
	}
	if len(chunks) == 0 {
		if status, ok := taskObj["status"].(map[string]any); ok {
			if message, ok := status["message"].(map[string]any); ok {
				if text := extractParts(message["parts"]); text != "" {
					chunks = append(chunks, text)
				}
			}
		}
	}
	if len(chunks) == 0 {
		return "", errors.New("multiagent: A2A task returned no text output")
	}
	return strings.Join(chunks, "\n"), nil
}

func extractParts(value any) string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		part, _ := item.(map[string]any)
		text, _ := part["text"].(string)
		if strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, "\n")
}
