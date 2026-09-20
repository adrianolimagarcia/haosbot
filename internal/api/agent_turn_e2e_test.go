package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider/openai"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type e2eTranscript struct {
	mu       sync.Mutex
	key      string
	messages []core.Message
}

func (t *e2eTranscript) Key() string { return t.key }

func (t *e2eTranscript) Messages() []core.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]core.Message, len(t.messages))
	copy(out, t.messages)
	return out
}

func (t *e2eTranscript) AddMessage(m core.Message) {
	t.mu.Lock()
	t.messages = append(t.messages, m)
	t.mu.Unlock()
}

func (t *e2eTranscript) Clear() {
	t.mu.Lock()
	t.messages = nil
	t.mu.Unlock()
}

func (t *e2eTranscript) Save() error { return nil }

type e2eTranscriptStore struct {
	mu sync.Mutex
	m  map[string]*e2eTranscript
}

func (s *e2eTranscriptStore) Open(key string) (agent.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*e2eTranscript{}
	}
	if t := s.m[key]; t != nil {
		return t, nil
	}
	t := &e2eTranscript{key: key}
	s.m[key] = t
	return t, nil
}

type e2eExecTool struct {
	tools.Base
	calls atomic.Int32
}

type e2eStreamingProvider struct{}

func (e2eStreamingProvider) Name() string { return "e2e-stream" }
func (e2eStreamingProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	return &core.Response{Content: "streamed", FinishReason: core.FinishStop, HasContent: true}, nil
}
func (e2eStreamingProvider) ChatStream(context.Context, provider.ChatRequest) (<-chan core.StreamEvent, error) {
	ch := make(chan core.StreamEvent, 2)
	ch <- core.StreamEvent{Kind: core.StreamText, Text: "stream"}
	ch <- core.StreamEvent{Kind: core.StreamDone, Response: &core.Response{Content: "streamed", FinishReason: core.FinishStop, HasContent: true}}
	close(ch)
	return ch, nil
}

func (t *e2eExecTool) Name() string { return "exec" }
func (t *e2eExecTool) Description() string { return "test command executor" }
func (t *e2eExecTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)
}
func (t *e2eExecTool) Execute(_ context.Context, args json.RawMessage) (tools.Result, error) {
	var input struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return tools.Errf("bad args: %v", err), nil
	}
	if input.Command != "printf 'e2e'" {
		return tools.Errf("unexpected command %q", input.Command), nil
	}
	t.calls.Add(1)
	return tools.OK("executed once"), nil
}

func TestAgentTurnEndpointXMLToolCallExecutesExactlyOnce(t *testing.T) {
	var providerCalls atomic.Int32
	var advertisedExec atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		if toolsValue, ok := request["tools"].([]any); ok {
			for _, raw := range toolsValue {
				if tool, ok := raw.(map[string]any); ok {
					fn, _ := tool["function"].(map[string]any)
					if fn["name"] == "exec" {
						advertisedExec.Add(1)
					}
				}
			}
		}
		n := providerCalls.Add(1)
		content := "done"
		if n == 1 {
			content = "<tool_call><function=exec><parameter=command>printf 'e2e'</parameter></function></tool_call>"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\x22choices\x22:[{\x22delta\x22:{\x22role\x22:\x22assistant\x22,\x22content\x22:%s},\x22finish_reason\x22:null}]}\n\n", mustJSONString(content))
		_, _ = io.WriteString(w, "data: {\x22choices\x22:[{\x22delta\x22:{},\x22finish_reason\x22:\x22stop\x22}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer providerServer.Close()

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Model = "xml-e2e-model"
	cfg.Agents.Defaults.Workspace = t.TempDir()
	client := openai.New(openai.Options{
		APIKey:         "test",
		BaseURL:        providerServer.URL + "/v1",
		Model:          "xml-e2e-model",
		ToolCallFormat: openai.ToolCallFormatXML,
	})
	execTool := &e2eExecTool{}
	registry := tools.NewRegistry()
	registry.Register(execTool)
	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus:         bus.New(bus.Options{}),
		Store:       &e2eTranscriptStore{},
		Provider:    client,
		Tools:       registry,
		Prompt:      prompt.New(cfg.Agents.Defaults.Workspace),
		Model:       cfg.Agents.Defaults.Model,
		SystemPrompt: "test",
		MaxIterations: 4,
		MaxTokens:    128,
		Workspace:   cfg.Agents.Defaults.Workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(cfg, client, loop)
	mux := http.NewServeMux()
	server.registerWebUI(mux)
	handler := server.securityMiddleware(mux)

	requestBody := `{"sessionId":"e2e-session-000001","message":"run the command"}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/turn", strings.NewReader(requestBody))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response agentTurnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Content != "done" {
		t.Fatalf("content=%q, want done", response.Content)
	}
	if got := execTool.calls.Load(); got != 1 {
		t.Fatalf("exec calls=%d, want exactly 1", got)
	}
	if got := providerCalls.Load(); got != 2 {
		t.Fatalf("provider calls=%d, want tool round plus final round", got)
	}
	if advertisedExec.Load() != 2 {
		t.Fatalf("exec was not advertised in both rounds: %d", advertisedExec.Load())
	}
}

func TestAgentTurnStreamEndpointForwardsStatefulDeltas(t *testing.T) {
	cfg := config.DefaultConfig()
	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus: bus.New(bus.Options{}), Store: &e2eTranscriptStore{},
		Provider: e2eStreamingProvider{}, Tools: tools.NewRegistry(),
		Prompt: prompt.New(t.TempDir()), Model: "stream-model",
		SystemPrompt: "test", MaxIterations: 2, MaxTokens: 64,
	})
	if err != nil { t.Fatal(err) }
	server := NewServer(cfg, e2eStreamingProvider{}, loop)
	mux := http.NewServeMux()
	server.registerWebUI(mux)
	handler := server.securityMiddleware(mux)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/turn/stream", strings.NewReader(`{"sessionId":"stream-session-0001","message":"hello"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK { t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String()) }
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	var events []agentTurnStreamEvent
	for scanner.Scan() {
		var event agentTurnStreamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil { t.Fatal(err) }
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil { t.Fatal(err) }
	if len(events) < 3 || events[0].Type != "context_snapshot" || events[1].Type != "text_delta" || events[1].Delta != "stream" {
		t.Fatalf("events=%+v, want context_snapshot followed by text delta", events)
	}
	last := events[len(events)-1]
	if last.Type != "done" || last.Content != "streamed" { t.Fatalf("last event=%+v", last) }
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type immediateTimeoutProvider struct{}

func (immediateTimeoutProvider) Name() string { return "timeout" }
func (immediateTimeoutProvider) Chat(ctx context.Context, _ provider.ChatRequest) (*core.Response, error) {
	return nil, context.DeadlineExceeded
}

func TestOpenAICompatProviderTimeoutIsStructured(t *testing.T) {
	cfg := config.DefaultConfig()
	server := NewServer(cfg, immediateTimeoutProvider{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["error"].(map[string]any); !ok {
		t.Fatalf("response is not structured: %s", rec.Body.String())
	}
}

type compatResponseProvider struct {
	request provider.ChatRequest
}

func (p *compatResponseProvider) Name() string { return "compat-test" }
func (p *compatResponseProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.request = req
	return &core.Response{
		ToolCalls: []core.ToolCall{{
			ID:        "call_compat",
			Name:      "exec",
			Arguments: json.RawMessage(`{"command":"printf 'ok'"}`),
		}},
		FinishReason: core.FinishToolCalls,
	}, nil
}

func TestOpenAICompatPreservesMultimodalMessagesAndToolCalls(t *testing.T) {
	cfg := config.DefaultConfig()
	providerStub := &compatResponseProvider{}
	server := NewServer(cfg, providerStub, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	body := `{"model":"compat-model","messages":[{"role":"user","content":[{"type":"text","text":"run"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}],"tools":[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}],"tool_choice":"required"}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(providerStub.request.Messages) != 1 || providerStub.request.Messages[0].Content.IsText() || len(providerStub.request.Messages[0].Content.Blocks) != 2 {
		t.Fatalf("multimodal content was not preserved: %+v", providerStub.request.Messages)
	}
	if len(providerStub.request.Tools) != 1 || providerStub.request.Tools[0].Name != "exec" {
		t.Fatalf("tools=%+v", providerStub.request.Tools)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	choices, _ := response["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v body=%s", calls, rec.Body.String())
	}
}
