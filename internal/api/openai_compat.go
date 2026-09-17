package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/command"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

type chatCompletionsRequest struct {
	Model               string          `json:"model"`
	Messages            []map[string]any `json:"messages"`
	Temperature         float64         `json:"temperature"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	Tools               []openAITool    `json:"tools"`
	ToolChoice          any             `json:"tool_choice"`
	Stream              bool            `json:"stream"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (t openAITool) providerSchema() (provider.ToolSchema, bool) {
	if strings.TrimSpace(t.Type) != "" && t.Type != "function" {
		return provider.ToolSchema{}, false
	}
	if strings.TrimSpace(t.Function.Name) == "" {
		return provider.ToolSchema{}, false
	}
	return provider.ToolSchema{
		Name:        t.Function.Name,
		Description: t.Function.Description,
		Parameters:  append(json.RawMessage(nil), t.Function.Parameters...),
	}, true
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		writeUnauthorized(w)
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "Method not allowed")
		return
	}

	var req chatCompletionsRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", fmt.Sprintf("Invalid JSON request: %v", err))
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
	// contract. Keep the legacy convenience only for one plain user message.
	if len(req.Messages) == 1 {
		last := req.Messages[0]
		content, _ := last["content"].(string)
		if s.cmdRouter != nil && command.IsSlashCommand(content) {
			reply, handled := s.cmdRouter.Execute(content, modelName, s.cfg.WorkspacePath())
			if handled {
				writeChatCompletionResponse(w, modelName, &core.Response{
					Content:      reply,
					HasContent:   reply != "",
					FinishReason: core.FinishStop,
				}, "chatcmpl-cmd")
				return
			}
		}
	}

	if s.provider == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "No LLM provider configured")
		return
	}

	coreMsgs := make([]core.Message, 0, len(req.Messages))
	for i, wireMsg := range req.Messages {
		raw, err := json.Marshal(wireMsg)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_message", fmt.Sprintf("message %d: %v", i, err))
			return
		}
		var msg core.Message
		if err := json.Unmarshal(raw, &msg); err != nil || !msg.Role.Valid() {
			if err == nil {
				err = fmt.Errorf("unsupported role %q", msg.Role)
			}
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_message", fmt.Sprintf("message %d: %v", i, err))
			return
		}
		coreMsgs = append(coreMsgs, msg)
	}
	if len(coreMsgs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "messages_required", "messages must not be empty")
		return
	}

	toolSchemas := make([]provider.ToolSchema, 0, len(req.Tools))
	for i, tool := range req.Tools {
		schema, ok := tool.providerSchema()
		if !ok {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_tool", fmt.Sprintf("tool %d must be a function tool with a name", i))
			return
		}
		toolSchemas = append(toolSchemas, schema)
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxCompletionTokens
	}
	chatReq := provider.ChatRequest{
		Messages:        coreMsgs,
		Tools:           toolSchemas,
		Model:           modelName,
		Temperature:     req.Temperature,
		MaxTokens:       maxTokens,
		ReasoningEffort: req.ReasoningEffort,
		ToolChoice:      req.ToolChoice,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	if req.Stream {
		s.handleChatCompletionStream(w, ctx, chatReq, modelName)
		return
	}

	chatResp, err := s.provider.Chat(ctx, chatReq)
	if err != nil {
		writeProviderAPIError(w, err)
		return
	}
	if chatResp == nil {
		writeAPIError(w, http.StatusBadGateway, "server_error", "empty_provider_response", "Provider returned an empty response")
		return
	}
	if chatResp.FinishReason == core.FinishError {
		writeResponseAPIError(w, chatResp)
		return
	}
	writeChatCompletionResponse(w, modelName, chatResp, "chatcmpl")
}

func (s *Server) handleChatCompletionStream(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest, modelName string) {
	sp, ok := s.provider.(provider.StreamingProvider)
	if !ok {
		resp, err := s.provider.Chat(ctx, req)
		if err != nil {
			writeProviderAPIError(w, err)
			return
		}
		if resp == nil {
			writeAPIError(w, http.StatusBadGateway, "server_error", "empty_provider_response", "Provider returned an empty response")
			return
		}
		writeSSEHeaders(w)
		id := completionID("chatcmpl")
		writeStreamChunk(w, id, modelName, map[string]any{"role": "assistant"}, "", nil)
		if resp.Content != "" {
			writeStreamChunk(w, id, modelName, map[string]any{"content": resp.Content}, "", nil)
		}
		writeStreamChunk(w, id, modelName, map[string]any{}, finishReason(resp), usageMap(resp.Usage))
		writeSSEDone(w)
		return
	}

	stream, err := sp.ChatStream(ctx, req)
	if err != nil {
		writeProviderAPIError(w, err)
		return
	}
	writeSSEHeaders(w)
	id := completionID("chatcmpl")
	writeStreamChunk(w, id, modelName, map[string]any{"role": "assistant"}, "", nil)

	for ev := range stream {
		switch ev.Kind {
		case core.StreamText:
			writeStreamChunk(w, id, modelName, map[string]any{"content": ev.Text}, "", nil)
		case core.StreamReasoning:
			writeStreamChunk(w, id, modelName, map[string]any{"reasoning_content": ev.Text}, "", nil)
		case core.StreamToolCall:
			tool := map[string]any{"index": ev.Index, "type": "function"}
			function := map[string]any{}
			if ev.ToolCallName != "" {
				function["name"] = ev.ToolCallName
			}
			if ev.ArgumentsDelta != "" {
				function["arguments"] = ev.ArgumentsDelta
			}
			if len(function) > 0 {
				tool["function"] = function
			}
			if ev.ToolCallID != "" {
				tool["id"] = ev.ToolCallID
			}
			writeStreamChunk(w, id, modelName, map[string]any{"tool_calls": []any{tool}}, "", nil)
		case core.StreamDone:
			if ev.Err != nil || ev.Response == nil || ev.Response.FinishReason == core.FinishError {
				writeSSEError(w, streamErrorResponse(ev))
				continue
			}
			writeStreamChunk(w, id, modelName, map[string]any{}, finishReason(ev.Response), usageMap(ev.Response.Usage))
		}
	}
	writeSSEDone(w)
}

func writeChatCompletion(w http.ResponseWriter, modelName, content, prefix string) {
	writeChatCompletionResponse(w, modelName, &core.Response{
		Content:      content,
		HasContent:   content != "",
		FinishReason: core.FinishStop,
	}, prefix)
}

func writeChatCompletionResponse(w http.ResponseWriter, modelName string, resp *core.Response, prefix string) {
	message := map[string]any{
		"role":    "assistant",
		"content": resp.Content,
	}
	if len(resp.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.ArgumentsString(),
				},
			})
		}
		message["tool_calls"] = calls
		if resp.Content == "" {
			message["content"] = nil
		}
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason(resp),
	}
	out := map[string]any{
		"id":      completionID(prefix),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]any{choice},
	}
	if usage := usageMap(resp.Usage); usage != nil {
		out["usage"] = usage
	}
	writeJSON(w, http.StatusOK, out)
}

func finishReason(resp *core.Response) string {
	if resp == nil || resp.FinishReason == "" {
		return "stop"
	}
	return string(resp.FinishReason)
}

func usageMap(usage *core.Usage) map[string]any {
	if usage == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
}

func completionID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, typ, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"code":    code,
		},
	})
}

func writeProviderAPIError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "provider_error"
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		code = "provider_timeout"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode >= 400 && httpErr.StatusCode <= 599 {
			status = httpErr.StatusCode
		}
		if httpErr.Retryable() {
			code = "provider_retryable_error"
		}
	}
	writeAPIError(w, status, "upstream_error", code, err.Error())
}

func writeResponseAPIError(w http.ResponseWriter, resp *core.Response) {
	status := http.StatusBadGateway
	if resp.ErrorStatusCode != nil && *resp.ErrorStatusCode >= 400 && *resp.ErrorStatusCode <= 599 {
		status = *resp.ErrorStatusCode
	}
	code := resp.ErrorCode
	if code == "" {
		code = resp.ErrorKind
	}
	if code == "" {
		code = "provider_error"
	}
	message := resp.Content
	if message == "" {
		message = "Provider returned an error response"
	}
	writeAPIError(w, status, "upstream_error", code, message)
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

func writeStreamChunk(w http.ResponseWriter, id, model string, delta map[string]any, finish string, usage map[string]any) {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	writeSSEData(w, chunk)
}

func writeSSEData(w http.ResponseWriter, value any) {
	payload, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEDone(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEError(w http.ResponseWriter, resp *core.Response) {
	message := "Provider stream failed"
	if resp != nil && resp.Content != "" {
		message = resp.Content
	}
	writeSSEData(w, map[string]any{"error": map[string]any{
		"message": message,
		"type":    "upstream_error",
		"code":    "provider_stream_error",
	}})
}

func streamErrorResponse(ev core.StreamEvent) *core.Response {
	if ev.Response != nil {
		return ev.Response
	}
	if ev.Err == nil {
		return &core.Response{Content: "Provider stream failed", FinishReason: core.FinishError}
	}
	return &core.Response{Content: ev.Err.Error(), FinishReason: core.FinishError}
}
