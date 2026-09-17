package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// capture records the single request a test server receives.
type capture struct {
	mu     sync.Mutex
	path   string
	header http.Header
	body   []byte
}

func (c *capture) record(r *http.Request) []byte {
	payload, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.header = r.Header.Clone()
	c.body = payload
	return payload
}

func decodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON object: %v (body=%s)", err, raw)
	}
	return out
}

func decodeArray(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON array: %v (raw=%s)", err, raw)
	}
	return out
}

func decodeValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON value: %v (raw=%s)", err, raw)
	}
	return out
}

func jsonEqual(t *testing.T, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("bad want JSON %q: %v", wantJSON, err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantEncoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantEncoded) {
		t.Fatalf("JSON mismatch:\n got: %s\nwant: %s", gotJSON, wantEncoded)
	}
}

func newClient(t *testing.T, baseURL string, opts ...func(*Options)) *Client {
	t.Helper()
	options := Options{APIKey: "test-key", BaseURL: baseURL, Model: "deepseek-chat"}
	for _, apply := range opts {
		apply(&options)
	}
	return New(options)
}

// collect drains a stream, returning every event and asserting the channel
// closes.
func collect(t *testing.T, stream <-chan core.StreamEvent) []core.StreamEvent {
	t.Helper()
	var events []core.StreamEvent
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-stream:
			if !ok {
				return events
			}
			events = append(events, event)
		case <-timeout:
			t.Fatal("stream channel did not close")
		}
	}
}

func doneResponse(t *testing.T, events []core.StreamEvent) *core.Response {
	t.Helper()
	var response *core.Response
	dones := 0
	for _, event := range events {
		if event.Kind == core.StreamDone {
			dones++
			if event.Response != nil {
				response = event.Response
			}
		}
	}
	if dones != 1 {
		t.Fatalf("expected exactly 1 StreamDone event, got %d", dones)
	}
	if response == nil {
		t.Fatal("StreamDone carried no response")
	}
	return response
}

func texts(events []core.StreamEvent, kind core.StreamEventKind) []string {
	var out []string
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event.Text)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Non-streaming happy path
// ---------------------------------------------------------------------------

func TestChatSendsReferenceRequestShape(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "chatcmpl-1",
			"object": "chat.completion",
			"model": "deepseek-chat",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"}],
			"usage": {
				"prompt_tokens": 12,
				"completion_tokens": 7,
				"total_tokens": 19,
				"prompt_tokens_details": {"cached_tokens": 4},
				"completion_tokens_details": {"reasoning_tokens": 3}
			}
		}`)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")

	user := core.NewMessage(core.RoleUser, "read a.txt")
	user.Timestamp = "2025-01-01T00:00:00"
	user.SetExtra("_hidden_history", json.RawMessage(`true`))

	assistant := &core.Message{
		Role:      core.RoleAssistant,
		Content:   core.TextContent(""),
		Timestamp: "2025-01-01T00:00:01",
		ToolCalls: []core.ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: json.RawMessage(`{"path":"a.txt"}`),
		}},
	}
	toolMsg := &core.Message{
		Role:       core.RoleTool,
		Content:    core.TextContent("file contents"),
		Timestamp:  "2025-01-01T00:00:02",
		ToolCallID: "call_1",
		Name:       "read_file",
	}

	req := provider.ChatRequest{
		Messages: []core.Message{
			*core.NewMessage(core.RoleSystem, "You are helpful."),
			*user,
			*assistant,
			*toolMsg,
		},
		Tools: []provider.ToolSchema{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		Model:       "deepseek-chat",
		MaxTokens:   256,
		Temperature: 0.3,
	}

	response, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// --- transport ---
	if cap.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", cap.path)
	}
	if got := cap.header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got)
	}
	if got := cap.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := cap.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	if cap.header.Get("x-session-affinity") == "" {
		t.Error("x-session-affinity header missing")
	}

	// --- request body ---
	body := decodeObject(t, cap.body)
	if got := string(body["model"]); got != `"deepseek-chat"` {
		t.Errorf("model = %s", got)
	}
	if got := string(body["max_tokens"]); got != "256" {
		t.Errorf("max_tokens = %s, want 256", got)
	}
	if got := string(body["temperature"]); got != "0.3" {
		t.Errorf("temperature = %s, want 0.3", got)
	}
	if got := string(body["tool_choice"]); got != `"auto"` {
		t.Errorf("tool_choice = %s, want \"auto\"", got)
	}
	// The reference's non-streaming chat() never passes `stream` to the SDK,
	// which drops omitted parameters, so the key must be absent.
	if _, present := body["stream"]; present {
		t.Errorf("non-streaming body must not carry a stream key, got %s", body["stream"])
	}
	if _, present := body["stream_options"]; present {
		t.Errorf("non-streaming body must not carry stream_options, got %s", body["stream_options"])
	}
	// No transcript-only field may leak into the request.
	for _, forbidden := range []string{"timestamp", "thinking_blocks", "_hidden_history"} {
		if strings.Contains(string(cap.body), forbidden) {
			t.Errorf("request body leaked %q: %s", forbidden, cap.body)
		}
	}

	tools := decodeArray(t, body["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools = %s", body["tools"])
	}
	jsonEqual(t, decodeValue(t, tools[0]["function"]), `{
		"name": "read_file",
		"description": "Read a file",
		"parameters": {"type":"object","properties":{"path":{"type":"string"}}}
	}`)

	messages := decodeArray(t, body["messages"])
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(messages), body["messages"])
	}
	// Every message carries only API-safe keys, in the reference's order.
	for i, msg := range messages {
		keys := make([]string, 0, len(msg))
		for key := range msg {
			keys = append(keys, key)
		}
		for _, key := range keys {
			if !allowedMessageKeys[key] {
				t.Errorf("message %d carries non-API key %q", i, key)
			}
		}
		if _, present := msg["timestamp"]; present {
			t.Errorf("message %d carries a timestamp", i)
		}
	}
	jsonEqual(t, decodeValue(t, messages[0]["role"]), `"system"`)
	jsonEqual(t, decodeValue(t, messages[0]["content"]), `"You are helpful."`)
	jsonEqual(t, decodeValue(t, messages[1]["content"]), `"read a.txt"`)

	// Assistant message: empty content becomes null because tool calls follow.
	if got := string(messages[2]["content"]); got != "null" {
		t.Errorf("assistant content = %s, want null", got)
	}
	calls := decodeArray(t, messages[2]["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %s", messages[2]["tool_calls"])
	}
	jsonEqual(t, calls[0], `{
		"id": "call_1",
		"type": "function",
		"function": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}
	}`)

	// Tool message: tool_call_id and name both survive.
	jsonEqual(t, decodeValue(t, messages[3]["role"]), `"tool"`)
	jsonEqual(t, decodeValue(t, messages[3]["tool_call_id"]), `"call_1"`)
	jsonEqual(t, decodeValue(t, messages[3]["name"]), `"read_file"`)

	// --- response mapping ---
	if response.Content != "Hello!" {
		t.Errorf("content = %q", response.Content)
	}
	if !response.HasContent {
		t.Error("HasContent = false")
	}
	if response.FinishReason != core.FinishStop {
		t.Errorf("finish_reason = %q", response.FinishReason)
	}
	if len(response.ToolCalls) != 0 {
		t.Errorf("unexpected tool calls: %+v", response.ToolCalls)
	}
	if response.Usage == nil {
		t.Fatal("usage = nil")
	}
	if response.Usage.PromptTokens != 12 || response.Usage.CompletionTokens != 7 || response.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v", response.Usage)
	}
	if response.Usage.CachedTokens == nil || *response.Usage.CachedTokens != 4 {
		t.Errorf("cached tokens = %v", response.Usage.CachedTokens)
	}
	if response.Usage.ReasoningTokens == nil || *response.Usage.ReasoningTokens != 3 {
		t.Errorf("reasoning tokens = %v", response.Usage.ReasoningTokens)
	}
}

// ---------------------------------------------------------------------------
// Tool-call parsing (non-streaming)
// ---------------------------------------------------------------------------

func TestChatParsesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": null,
					"reasoning_content": "weighing options",
					"tool_calls": [
						{"id": "call_a", "type": "function", "function": {"name": "list_dir", "arguments": "{\"path\":\".\"}"}},
						{"id": "call_a", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}},
						{"id": "", "type": "function", "function": {"name": "no_args"}}
					]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2}
		}`)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	response, err := client.Chat(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if response.FinishReason != core.FinishToolCalls {
		t.Errorf("finish_reason = %q", response.FinishReason)
	}
	if response.ReasoningContent != "weighing options" {
		t.Errorf("reasoning_content = %q", response.ReasoningContent)
	}
	if !response.ShouldExecuteTools() {
		t.Error("ShouldExecuteTools() = false")
	}
	if len(response.ToolCalls) != 3 {
		t.Fatalf("tool calls = %d", len(response.ToolCalls))
	}
	if response.ToolCalls[0].Name != "list_dir" || string(response.ToolCalls[0].Arguments) != `{"path":"."}` {
		t.Errorf("call 0 = %+v", response.ToolCalls[0])
	}
	if response.ToolCalls[2].Name != "no_args" || string(response.ToolCalls[2].Arguments) != "{}" {
		t.Errorf("call 2 = %+v (arguments should default to {})", response.ToolCalls[2])
	}
	// The first id is kept; a duplicate or empty id is replaced by a fresh
	// 9-character short id so replayed tool results cannot collide.
	if response.ToolCalls[0].ID != "call_a" {
		t.Errorf("first id = %q, want call_a", response.ToolCalls[0].ID)
	}
	ids := map[string]bool{}
	for _, call := range response.ToolCalls {
		if ids[call.ID] {
			t.Errorf("duplicate tool call id %q", call.ID)
		}
		ids[call.ID] = true
	}
	for _, call := range response.ToolCalls[1:] {
		if call.ID == "call_a" {
			t.Errorf("duplicate id %q was not replaced", call.ID)
		}
		if len(call.ID) != 9 {
			t.Errorf("replacement id %q is not 9 characters", call.ID)
		}
	}
	// The usage total is at least prompt + completion (LLMUsage.reported).
	if response.Usage == nil || response.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v", response.Usage)
	}
}

func TestChatLiftsTextToolCalls(t *testing.T) {
	fence := "```"
	// A provider answering in text-format tool-call blocks, fenced with ```json.
	content := "Sure.\n<tool_call>\n" + fence + "json\n" +
		`{"name":"read_file","arguments":{"path":"a.txt"}}` + "\n" + fence + "\n</tool_call>"
	payload, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	response, err := client.Chat(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if response.Content != "Sure." {
		t.Errorf("content = %q, want %q", response.Content, "Sure.")
	}
	if len(response.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", response.ToolCalls)
	}
	if response.ToolCalls[0].Name != "read_file" || string(response.ToolCalls[0].Arguments) != `{"path":"a.txt"}` {
		t.Errorf("call = %+v", response.ToolCalls[0])
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

const streamBody = `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"let me "}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hel"}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"content":"lo","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`

func TestChatStreamAggregatesDeltas(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, line := range strings.SplitAfter(streamBody, "\n") {
			_, _ = io.WriteString(w, line)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	stream, err := client.ChatStream(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "read a.txt")},
		Tools:    []provider.ToolSchema{{Name: "read_file", Description: "Read", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	events := collect(t, stream)

	// --- request body ---
	body := decodeObject(t, cap.body)
	if got := string(body["stream"]); got != "true" {
		t.Errorf("stream = %s, want true", got)
	}
	streamOptions := decodeObject(t, body["stream_options"])
	if got := string(streamOptions["include_usage"]); got != "true" {
		t.Errorf("stream_options.include_usage = %s, want true", got)
	}

	// --- events ---
	if got := texts(events, core.StreamText); strings.Join(got, "") != "Hello" {
		t.Errorf("text deltas = %q", got)
	}
	if got := texts(events, core.StreamReasoning); strings.Join(got, "") != "let me " {
		t.Errorf("reasoning deltas = %q", got)
	}
	var toolEvents []core.StreamEvent
	for _, event := range events {
		if event.Kind == core.StreamToolCall {
			toolEvents = append(toolEvents, event)
		}
	}
	if len(toolEvents) != 3 {
		t.Fatalf("tool call events = %d (%+v)", len(toolEvents), toolEvents)
	}
	if toolEvents[0].Index != 0 || toolEvents[0].ToolCallID != "call_a" || toolEvents[0].ToolCallName != "read_file" {
		t.Errorf("first tool fragment = %+v", toolEvents[0])
	}
	if toolEvents[1].ArgumentsDelta != `{"path":` || toolEvents[2].ArgumentsDelta != `"a.txt"}` {
		t.Errorf("argument fragments = %q, %q", toolEvents[1].ArgumentsDelta, toolEvents[2].ArgumentsDelta)
	}
	var usageEvents int
	for _, event := range events {
		if event.Kind == core.StreamUsage {
			usageEvents++
			if event.Usage == nil || event.Usage.TotalTokens != 15 {
				t.Errorf("usage event = %+v", event.Usage)
			}
		}
	}
	if usageEvents != 1 {
		t.Errorf("usage events = %d, want 1", usageEvents)
	}

	// --- aggregated response ---
	response := doneResponse(t, events)
	if response.Content != "Hello" {
		t.Errorf("content = %q, want Hello", response.Content)
	}
	if response.ReasoningContent != "let me " {
		t.Errorf("reasoning = %q", response.ReasoningContent)
	}
	if response.FinishReason != core.FinishToolCalls {
		t.Errorf("finish_reason = %q", response.FinishReason)
	}
	if len(response.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", response.ToolCalls)
	}
	call := response.ToolCalls[0]
	if call.ID != "call_a" || call.Name != "read_file" {
		t.Errorf("call = %+v", call)
	}
	if string(call.Arguments) != `{"path":"a.txt"}` {
		t.Errorf("assembled arguments = %s", call.Arguments)
	}
	if !json.Valid(call.Arguments) {
		t.Errorf("arguments are not valid JSON: %s", call.Arguments)
	}
	if response.Usage == nil || response.Usage.PromptTokens != 10 || response.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", response.Usage)
	}
}

func TestChatStreamStopsAtDoneAndRequiresFinishReason(t *testing.T) {
	t.Run("done terminates the stream", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			// Anything after [DONE] must be ignored.
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"IGNORED\"}}]}\n\n")
		}))
		defer srv.Close()

		client := newClient(t, srv.URL+"/v1")
		stream, err := client.ChatStream(context.Background(), provider.ChatRequest{
			Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		events := collect(t, stream)
		response := doneResponse(t, events)
		if response.Content != "hi" {
			t.Errorf("content = %q", response.Content)
		}
		if strings.Contains(response.Content, "IGNORED") {
			t.Error("content after [DONE] was aggregated")
		}
	})

	t.Run("stream without a finish reason fails like the reference", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		client := newClient(t, srv.URL+"/v1")
		stream, err := client.ChatStream(context.Background(), provider.ChatRequest{
			Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		events := collect(t, stream)
		response := doneResponse(t, events)
		if response.FinishReason != core.FinishError {
			t.Errorf("finish_reason = %q, want error", response.FinishReason)
		}
		if response.ErrorKind != "connection" {
			t.Errorf("error_kind = %q, want connection", response.ErrorKind)
		}
		if !strings.Contains(response.Content, errStreamEndedEarly) {
			t.Errorf("content = %q", response.Content)
		}
	})
}

func TestChatStreamAcceptsNonCanonicalFraming(t *testing.T) {
	chunk := `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`
	cases := []struct {
		name string
		body string
	}{
		{"canonical blank-line framing", "data: " + chunk + "\n\ndata: [DONE]\n\n"},
		{"single newline framing", "data: " + chunk + "\ndata: [DONE]\n"},
		{"crlf framing", "data: " + chunk + "\r\n\r\ndata: [DONE]\r\n\r\n"},
		{"comment keep-alive", ": ping\n\ndata: " + chunk + "\n\ndata: [DONE]\n\n"},
		{"unterminated final event", "data: " + chunk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			client := newClient(t, srv.URL+"/v1")
			stream, err := client.ChatStream(context.Background(), provider.ChatRequest{
				Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
			})
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			response := doneResponse(t, collect(t, stream))
			if response.Content != "ok" || response.FinishReason != core.FinishStop {
				t.Fatalf("response = %+v", response)
			}
		})
	}
}

func TestChatStreamInBandErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"upstream exploded\",\"type\":\"server_error\"}}\n\n")
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	stream, err := client.ChatStream(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	response := doneResponse(t, collect(t, stream))
	if response.FinishReason != core.FinishError {
		t.Errorf("finish_reason = %q", response.FinishReason)
	}
	if !strings.Contains(response.Content, "upstream exploded") {
		t.Errorf("content = %q", response.Content)
	}
}

func TestChatStreamContextCancellationClosesChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		// Block until the client goes away.
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.ChatStream(ctx, provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
	})
	if err != nil {
		cancel()
		t.Fatalf("ChatStream: %v", err)
	}

	select {
	case event := <-stream:
		if event.Kind != core.StreamText || event.Text != "first" {
			cancel()
			t.Fatalf("first event = %+v", event)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no event received before cancellation")
	}

	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-stream:
			if !ok {
				return // channel closed promptly: contract satisfied
			}
		case <-deadline:
			t.Fatal("stream channel did not close after context cancellation")
		}
	}
}

func TestChatStreamIdleTimeout(t *testing.T) {
	t.Setenv(streamIdleTimeoutEnv, "0.2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	stream, err := client.ChatStream(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	response := doneResponse(t, collect(t, stream))
	if response.FinishReason != core.FinishError || response.ErrorKind != "timeout" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(response.Content, "stream stalled for more than 0.2 seconds") {
		t.Errorf("content = %q", response.Content)
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func TestHTTPErrorIsTypedAndCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Retry-After", "2.5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1")
	req := provider.ChatRequest{Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")}}

	_, err := client.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error")
	}
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error is %T, want *provider.HTTPError", err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d", httpErr.StatusCode)
	}
	if httpErr.RetryAfter != 2.5 {
		t.Errorf("retry-after = %v, want 2.5", httpErr.RetryAfter)
	}
	if !httpErr.Retryable() {
		t.Error("429 must be retryable")
	}
	if !strings.Contains(httpErr.Body, "rate limited") {
		t.Errorf("body = %q", httpErr.Body)
	}

	// The streaming entry point reports the same typed error.
	stream, err := client.ChatStream(context.Background(), req)
	if stream != nil {
		t.Error("ChatStream returned a channel on failure")
	}
	if !errors.As(err, &httpErr) {
		t.Fatalf("ChatStream error is %T, want *provider.HTTPError", err)
	}
}

func TestRetryAfterHeaderForms(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    float64
	}{
		{"milliseconds", map[string]string{"Retry-After-Ms": "1500"}, 1.5},
		{"seconds", map[string]string{"Retry-After": "3"}, 3},
		{"zero clamps to the reference floor", map[string]string{"Retry-After": "0"}, 0.1},
		{"absent", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			for key, value := range tc.headers {
				header.Set(key, value)
			}
			if got := retryAfterFromHeaders(header); got != tc.want {
				t.Errorf("retryAfterFromHeaders = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Base URL joining
// ---------------------------------------------------------------------------

func TestBaseURLJoining(t *testing.T) {
	for _, suffix := range []string{"/v1", "/v1/"} {
		t.Run(suffix, func(t *testing.T) {
			cap := &capture{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cap.record(r)
				_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			client := newClient(t, srv.URL+suffix)
			if _, err := client.Chat(context.Background(), provider.ChatRequest{
				Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if cap.path != "/v1/chat/completions" {
				t.Errorf("path = %q, want /v1/chat/completions", cap.path)
			}
		})
	}
}

func TestExtraHeadersOverrideDefaults(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/v1", func(o *Options) {
		o.ExtraHeaders = map[string]string{"X-Custom": "1", "Authorization": "Bearer override"}
	})
	if _, err := client.Chat(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := cap.header.Get("X-Custom"); got != "1" {
		t.Errorf("X-Custom = %q", got)
	}
	if got := cap.header.Get("Authorization"); got != "Bearer override" {
		t.Errorf("Authorization = %q, want the caller's override", got)
	}
}

// ---------------------------------------------------------------------------
// Request-building rules
// ---------------------------------------------------------------------------

func TestBuildBodyModelRules(t *testing.T) {
	cases := []struct {
		name          string
		model         string
		effort        string
		wantMaxKey    string
		wantTemp      bool
		wantEffortKey string
	}{
		{"plain model", "deepseek-chat", "", "max_tokens", true, ""},
		{"reasoning disables temperature", "deepseek-chat", "high", "max_tokens", false, "high"},
		{"effort none keeps temperature", "deepseek-chat", "none", "max_tokens", true, ""},
		{"kimi-k3 uses max_completion_tokens", "kimi-k3", "high", "max_completion_tokens", false, "max"},
		{"kimi-k3 drops disabled reasoning", "kimi-k3", "none", "max_completion_tokens", false, ""},
		{"gpt-5 uses max_completion_tokens", "gpt-5.1", "", "max_completion_tokens", false, ""},
		{"o-series uses max_completion_tokens", "o3-mini", "", "max_completion_tokens", false, ""},
		{"routed model name", "openai/gpt-5.1", "", "max_completion_tokens", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClient(t, "http://127.0.0.1:1/v1")
			body, err := client.buildBody(provider.ChatRequest{
				Messages:        []core.Message{*core.NewMessage(core.RoleUser, "hi")},
				Model:           tc.model,
				ReasoningEffort: tc.effort,
			}, false)
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			decoded := decodeObject(t, body)
			if _, ok := decoded[tc.wantMaxKey]; !ok {
				t.Errorf("missing %s in %s", tc.wantMaxKey, body)
			}
			if _, ok := decoded["temperature"]; ok != tc.wantTemp {
				t.Errorf("temperature present = %v, want %v (%s)", ok, tc.wantTemp, body)
			}
			effort, hasEffort := decoded["reasoning_effort"]
			if tc.wantEffortKey == "" {
				if hasEffort {
					t.Errorf("unexpected reasoning_effort = %s", effort)
				}
			} else if string(effort) != `"`+tc.wantEffortKey+`"` {
				t.Errorf("reasoning_effort = %s, want %q", effort, tc.wantEffortKey)
			}
		})
	}
}

func TestBuildBodyBackfillsReasoningContentForThinkingModels(t *testing.T) {
	client := newClient(t, "http://127.0.0.1:1/v1")
	body, err := client.buildBody(provider.ChatRequest{
		Messages: []core.Message{
			*core.NewMessage(core.RoleUser, "hi"),
			*core.NewMessage(core.RoleAssistant, "hello"),
			*core.NewMessage(core.RoleUser, "again"),
		},
		Model:           "qwen3.6-flash",
		ReasoningEffort: "high",
	}, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	messages := decodeArray(t, decodeObject(t, body)["messages"])
	if got := string(messages[1]["reasoning_content"]); got != `""` {
		t.Errorf("assistant reasoning_content = %s, want \"\"", got)
	}
}

func TestMultimodalContentBlocksArePreserved(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	image := core.ContentBlock{
		Type:     "image_url",
		ImageURL: json.RawMessage(`{"url":"data:image/png;base64,AAAA"}`),
	}
	message := &core.Message{
		Role: core.RoleUser,
		Content: core.BlockContent([]core.ContentBlock{
			{Type: "text", Text: "what is this?"},
			image,
		}),
	}

	client := newClient(t, srv.URL+"/v1")
	if _, err := client.Chat(context.Background(), provider.ChatRequest{
		Messages: []core.Message{*message},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	messages := decodeArray(t, decodeObject(t, cap.body)["messages"])
	blocks := decodeArray(t, messages[0]["content"])
	if len(blocks) != 2 {
		t.Fatalf("blocks = %s", messages[0]["content"])
	}
	jsonEqual(t, blocks[0], `{"type":"text","text":"what is this?"}`)
	jsonEqual(t, blocks[1], `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`)
}

func TestEnforceRoleAlternation(t *testing.T) {
	t.Run("merges consecutive user turns", func(t *testing.T) {
		messages := []core.Message{
			*core.NewMessage(core.RoleUser, "first"),
			*core.NewMessage(core.RoleUser, "second"),
			*core.NewMessage(core.RoleAssistant, "answer"),
			*core.NewMessage(core.RoleUser, "third"),
		}
		out, err := sanitizeMessages(messages, "deepseek-chat", "")
		if err != nil {
			t.Fatalf("sanitizeMessages: %v", err)
		}
		if len(out) != 3 {
			t.Fatalf("messages = %d (%s)", len(out), out)
		}
		decoded := decodeArray(t, json.RawMessage("["+strings.Join(rawStrings(out), ",")+"]"))
		if got := string(decoded[0]["content"]); got != `"first\n\nsecond"` {
			t.Errorf("merged content = %s", got)
		}
	})

	t.Run("drops a trailing assistant turn", func(t *testing.T) {
		messages := []core.Message{
			*core.NewMessage(core.RoleUser, "hi"),
			*core.NewMessage(core.RoleAssistant, "prefill"),
		}
		out, err := sanitizeMessages(messages, "deepseek-chat", "")
		if err != nil {
			t.Fatalf("sanitizeMessages: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("messages = %d (%s)", len(out), out)
		}
		if !strings.Contains(string(out[0]), `"role":"user"`) {
			t.Errorf("message = %s", out[0])
		}
	})
}

func rawStrings(values []json.RawMessage) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func TestParseToolArgumentsShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"object", `{"a":1}`, `{"a":1}`},
		{"empty", ``, `{}`},
		{"null", `null`, `{}`},
		{"empty string", `""`, `{}`},
		{"stringified object", `"{\"a\":1}"`, `{"a":1}`},
		{"array is preserved", `[1,2]`, `[1,2]`},
		{"malformed is preserved", `"{oops"`, `"{oops"`},
		{"null string stays a string", `"null"`, `"null"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(parseToolArguments(json.RawMessage(tc.in)))
			if got != tc.want {
				t.Errorf("parseToolArguments(%s) = %s, want %s", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("result %s is not valid JSON", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Interface conformance
// ---------------------------------------------------------------------------

func TestClientSatisfiesProviderInterfaces(t *testing.T) {
	var _ provider.Provider = New(Options{})
	var _ provider.StreamingProvider = New(Options{})
	if got := New(Options{}).Name(); got != "openai" {
		t.Errorf("Name() = %q", got)
	}
	// Defaults mirror the reference: gpt-4o and the OpenAI base URL.
	client := New(Options{})
	if client.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q", client.baseURL)
	}
	if client.model != DefaultModel {
		t.Errorf("model = %q", client.model)
	}
	if client.apiKey != noKeyPlaceholder {
		t.Errorf("apiKey = %q, want %q", client.apiKey, noKeyPlaceholder)
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:11434/v1":      true,
		"http://127.0.0.1:8000/v1":       true,
		"http://192.168.1.10:8000/v1":    true,
		"http://host.docker.internal/v1": true,
		"https://api.deepseek.com/v1":    false,
		"https://openrouter.ai/api/v1":   false,
		"http://[::1]:11434/v1":          true,
		"http://10.0.0.5:8080/v1":        true,
	}
	for input, want := range cases {
		if got := isLocalEndpoint(input); got != want {
			t.Errorf("isLocalEndpoint(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestResolveStreamIdleTimeout(t *testing.T) {
	t.Setenv(streamIdleTimeoutEnv, "")
	if got := resolveStreamIdleTimeout(); got != defaultStreamIdleTimeout {
		t.Errorf("default = %v", got)
	}
	t.Setenv(streamIdleTimeoutEnv, "not-a-number")
	if got := resolveStreamIdleTimeout(); got != defaultStreamIdleTimeout {
		t.Errorf("invalid = %v", got)
	}
	t.Setenv(streamIdleTimeoutEnv, "-5")
	if got := resolveStreamIdleTimeout(); got != defaultStreamIdleTimeout {
		t.Errorf("non-positive = %v", got)
	}
	t.Setenv(streamIdleTimeoutEnv, "100000")
	if got := resolveStreamIdleTimeout(); got != maxStreamIdleTimeout {
		t.Errorf("clamped = %v", got)
	}
	t.Setenv(streamIdleTimeoutEnv, "12.5")
	if got := resolveStreamIdleTimeout(); got != 12500*time.Millisecond {
		t.Errorf("parsed = %v", got)
	}
}
