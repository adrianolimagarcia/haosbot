package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/command"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

type chatCompletionsRequest struct {
	Model       string           `json:"model"`
	Messages    []map[string]any `json:"messages"`
	Temperature float64          `json:"temperature"`
	MaxTokens   int              `json:"max_tokens"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		writeUnauthorized(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req chatCompletionsRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON request: %v", err), http.StatusBadRequest)
		return
	}

	modelName := req.Model
	if modelName == "" {
		modelName = s.cfg.Agents.Defaults.Model
		if modelName == "" {
			modelName = "deepseek-chat"
		}
	}

	// Slash commands are local HAOSBot behavior, not part of the OpenAI wire
	// contract. Keep the legacy convenience only when the request consists of a
	// single user message; never mix it with persisted session state.
	if len(req.Messages) == 1 {
		last := req.Messages[0]
		content, _ := last["content"].(string)
		if s.cmdRouter != nil && command.IsSlashCommand(content) {
			reply, handled := s.cmdRouter.Execute(content, modelName, s.cfg.WorkspacePath())
			if handled {
				writeChatCompletion(w, modelName, reply, "chatcmpl-cmd")
				return
			}
		}
	}

	if s.provider == nil {
		http.Error(w, "No LLM provider configured", http.StatusServiceUnavailable)
		return
	}

	coreMsgs := make([]core.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		coreMsgs = append(coreMsgs, core.Message{
			Role:    core.Role(role),
			Content: core.TextContent(content),
		})
	}
	if len(coreMsgs) == 0 {
		http.Error(w, "messages must not be empty", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	chatResp, err := s.provider.Chat(ctx, provider.ChatRequest{
		Messages:    coreMsgs,
		Model:       modelName,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Provider error: %v", err), http.StatusInternalServerError)
		return
	}
	writeChatCompletion(w, modelName, chatResp.Content, "chatcmpl")
}

func writeChatCompletion(w http.ResponseWriter, modelName, content, prefix string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
	})
}
