package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// scriptedProvider returns a pre-programmed response per call.
type scriptedProvider struct {
	mu        sync.Mutex
	responses []*core.Response
	calls     int
	requests  []provider.ChatRequest
	err       error
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.err != nil {
		return nil, p.err
	}
	if p.calls >= len(p.responses) {
		return &core.Response{Content: "fallback", FinishReason: core.FinishStop}, nil
	}
	r := p.responses[p.calls]
	p.calls++
	return r, nil
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// streamingProvider streams a scripted response.
type streamingProvider struct {
	scriptedProvider
}

func (p *streamingProvider) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan core.StreamEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	idx := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan core.StreamEvent, 16)
	go func() {
		defer close(ch)
		if idx >= len(p.responses) {
			ch <- core.StreamEvent{Kind: core.StreamText, Text: "fallback"}
			ch <- core.StreamEvent{Kind: core.StreamDone, Response: &core.Response{
				Content: "fallback", FinishReason: core.FinishStop}}
			return
		}
		r := p.responses[idx]
		for _, part := range chunkString(r.Content, 3) {
			select {
			case ch <- core.StreamEvent{Kind: core.StreamText, Text: part}:
			case <-ctx.Done():
				return
			}
		}
		if r.ReasoningContent != "" {
			ch <- core.StreamEvent{Kind: core.StreamReasoning, Text: r.ReasoningContent}
		}
		for i, tc := range r.ToolCalls {
			ch <- core.StreamEvent{Kind: core.StreamToolCall, Index: i, ToolCallID: tc.ID, ToolCallName: tc.Name}
			ch <- core.StreamEvent{Kind: core.StreamToolCall, Index: i, ArgumentsDelta: string(tc.Arguments)}
		}
		ch <- core.StreamEvent{Kind: core.StreamDone, Response: r}
	}()
	return ch, nil
}

func chunkString(s string, n int) []string {
	if s == "" {
		return nil
	}
	var out []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// echoTool is a read-only, concurrency-safe tool.
type echoTool struct {
	tools.ReadOnlyBase
	name    string
	delay   time.Duration
	mu      sync.Mutex
	calls   int
	failErr bool
}

func (t *echoTool) Name() string        { return t.name }
func (t *echoTool) Description() string { return "echoes input" }
func (t *echoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}}}`)
}

func (t *echoTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-ctx.Done():
			return tools.Result{}, ctx.Err()
		}
	}
	if t.failErr {
		return tools.Result{}, errors.New("tool blew up")
	}
	var m map[string]any
	_ = json.Unmarshal(args, &m)
	return tools.OK(fmt.Sprintf("%s:%v", t.name, m["v"])), nil
}

// writeTool is a non-concurrency-safe tool (mutating).
type writeTool struct {
	tools.Base
	mu    sync.Mutex
	calls int
}

func (t *writeTool) Name() string                { return "write_thing" }
func (t *writeTool) Description() string         { return "writes" }
func (t *writeTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *writeTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return tools.OK("written"), nil
}

func toolCall(id, name, args string) core.ToolCall {
	return core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRunSimpleFinalAnswer(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "hello world", FinishReason: core.FinishStop},
	}}
	r := NewRunner()
	res, err := r.Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "hello world" {
		t.Errorf("content = %q", res.FinalContent)
	}
	if res.StopReason != StopCompleted {
		t.Errorf("stop = %q", res.StopReason)
	}
	if p.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1", p.callCount())
	}
	// Transcript: user, assistant.
	if len(res.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(res.Messages))
	}
	if res.Messages[1].Role != core.RoleAssistant {
		t.Errorf("last role = %q", res.Messages[1].Role)
	}
}

func TestRunToolLoopReinjectsResult(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "", FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{toolCall("c1", "echo", `{"v":"x"}`)}},
		{Content: "done", FinishReason: core.FinishStop},
	}}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	r := NewRunner()
	res, err := r.Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "done" {
		t.Errorf("content = %q", res.FinalContent)
	}
	if p.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", p.callCount())
	}
	if len(res.ToolsUsed) != 1 || res.ToolsUsed[0] != "echo" {
		t.Errorf("tools used = %v", res.ToolsUsed)
	}

	// The second request must carry the assistant tool_call and the tool result.
	second := p.requests[1]
	var sawAssistantCall, sawToolResult bool
	for _, m := range second.Messages {
		if m.Role == core.RoleAssistant && len(m.ToolCalls) == 1 {
			sawAssistantCall = true
		}
		if m.Role == core.RoleTool {
			// tool_call_id is a modelled field; assert the field itself, since
			// that is what MarshalJSON emits and what the provider transmits.
			if m.ToolCallID != "c1" {
				t.Errorf("tool message tool_call_id = %q, want %q: %+v", m.ToolCallID, "c1", m)
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal tool message: %v", err)
			}
			if !strings.Contains(string(raw), `"tool_call_id":"c1"`) {
				t.Errorf("tool_call_id missing from serialized tool message: %s", raw)
			}
			if m.Content.Text != "echo:x" {
				t.Errorf("tool content = %q", m.Content.Text)
			}
			sawToolResult = true
		}
	}
	if !sawAssistantCall {
		t.Error("assistant tool_call not reinjected")
	}
	if !sawToolResult {
		t.Error("tool result not reinjected")
	}
}

func TestRunMaxIterations(t *testing.T) {
	// Provider always asks for a tool -> never finishes.
	p := &scriptedProvider{}
	p.responses = nil
	p.err = nil
	always := func(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
		return &core.Response{FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{toolCall("c", "echo", `{"v":"1"}`)}}, nil
	}
	_ = always

	loop := &loopProvider{}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	r := NewRunner()
	res, err := r.Run(context.Background(), RunSpec{
		Messages:      []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider:      loop,
		Tools:         reg,
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopMaxIterations {
		t.Errorf("stop = %q, want %q", res.StopReason, StopMaxIterations)
	}
	if !strings.Contains(res.FinalContent, "maximum number of tool call iterations (3)") {
		t.Errorf("unexpected budget message: %q", res.FinalContent)
	}
	// Three tool-call iterations, plus one budget-exhausted finalization
	// attempt. The reference makes that extra request by default
	// (finalize_on_max_iterations=True, runner.py:112); the attempt is
	// rejected here because the provider keeps asking for tools, so the
	// static notice is used.
	if loop.calls != 4 {
		t.Errorf("provider calls = %d, want 4 (3 iterations + 1 finalization attempt)", loop.calls)
	}
}

// loopProvider always requests the same tool.
type loopProvider struct{ calls int }

func (p *loopProvider) Name() string { return "loop" }
func (p *loopProvider) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.calls++
	return &core.Response{FinishReason: core.FinishToolCalls,
		ToolCalls: []core.ToolCall{toolCall("c", "echo", `{"v":"1"}`)}}, nil
}

func TestToolResultTruncation(t *testing.T) {
	big := strings.Repeat("A", 500)
	truncTool := &bigTool{out: big}
	reg := tools.NewRegistry()
	reg.Register(truncTool)

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{toolCall("c", "big", `{}`)}},
		{Content: "ok", FinishReason: core.FinishStop},
	}}

	r := NewRunner()
	res, err := r.Run(context.Background(), RunSpec{
		Messages:           []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider:           p,
		Tools:              reg,
		MaxToolResultChars: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var toolMsg string
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			toolMsg = m.Content.Text
		}
	}
	// max_chars plus the suffix: the reference cuts to the limit and THEN
	// appends, so the result is longer than the limit by design.
	if len([]rune(toolMsg)) > 100+len(textutil.TruncatedSuffix) {
		t.Errorf("tool result not truncated: %d chars", len([]rune(toolMsg)))
	}
	// The reference's marker is _TRUNCATED_SUFFIX = "\n... (truncated)"
	// (helpers.py:371). It deliberately does NOT state the original size; an
	// earlier port invented a richer marker, which diverged from the
	// reference and would have confused a differential comparison.
	if !strings.Contains(toolMsg, "... (truncated)") {
		t.Errorf("missing the reference truncation marker: %q", toolMsg)
	}
}

type bigTool struct {
	tools.ReadOnlyBase
	out string
}

func (t *bigTool) Name() string                { return "big" }
func (t *bigTool) Description() string         { return "big" }
func (t *bigTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *bigTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	return tools.OK(t.out), nil
}

func TestToolErrorBecomesContent(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo", failErr: true})

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{toolCall("c", "echo", `{}`)}},
		{Content: "recovered", FinishReason: core.FinishStop},
	}}

	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The run must continue rather than abort.
	if res.FinalContent != "recovered" {
		t.Errorf("content = %q", res.FinalContent)
	}
	var toolMsg string
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			toolMsg = m.Content.Text
		}
	}
	if !strings.Contains(toolMsg, "tool blew up") {
		t.Errorf("error not surfaced to model: %q", toolMsg)
	}
}

func TestUnknownToolBecomesErrorResult(t *testing.T) {
	reg := tools.NewRegistry()
	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{toolCall("c", "nope", `{}`)}},
		{Content: "ok", FinishReason: core.FinishStop},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	var toolMsg string
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			toolMsg = m.Content.Text
		}
	}
	if !strings.Contains(toolMsg, "unknown tool") {
		t.Errorf("got %q", toolMsg)
	}
}

// TestDegenerateToolCallDropped is the regression test for the session-wedging
// bug: a call with an empty name must never be executed or persisted.
func TestDegenerateToolCallDropped(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{{ID: "c", Name: "", Arguments: json.RawMessage(`{}`)}}},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopCompleted {
		t.Errorf("stop = %q", res.StopReason)
	}
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			t.Errorf("degenerate call was executed: %+v", m)
		}
	}
	if p.callCount() != 1 {
		t.Errorf("should not loop on degenerate call, calls=%d", p.callCount())
	}
}

func TestEmptyContentRetry(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "", FinishReason: core.FinishStop},
		{Content: "", FinishReason: core.FinishStop},
		{Content: "finally", FinishReason: core.FinishStop},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "finally" {
		t.Errorf("content = %q, want finally", res.FinalContent)
	}
	if p.callCount() != 3 {
		t.Errorf("calls = %d, want 3", p.callCount())
	}
}

// TestEmptyContentGivesUpAfterRetries pins the reference's terminal blank
// branch (runner.py:737-754) rather than the port's old behaviour of returning
// the empty string.
//
// The expectation used to be `content == ""` with stop reason "completed",
// which pinned the port's own output; the reference never returns an empty
// answer. It substitutes EMPTY_FINAL_RESPONSE_MESSAGE, reports stop_reason
// "empty_final_response", sets the run's error to the same text, and writes the
// notice into the transcript through _append_final_message (runner.py:738-745).
//
// Verified by executing the frozen reference on exactly this script (four blank
// responses, so the fourth is never consumed): final_content and error are the
// notice, stop_reason is "empty_final_response", the provider is called three
// times, and the transcript is
//
//	[{"role":"user","content":"go"},
//	 {"role":"assistant","content":"I completed the tool steps but couldn't ..."}]
//
// — one assistant turn carrying the NOTICE, not a blank one.
// compat/runner_terminal_differential_test.go re-checks this against the frozen
// runner over the whole blankness corpus.
func TestEmptyContentGivesUpAfterRetries(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "", FinishReason: core.FinishStop},
		{Content: "", FinishReason: core.FinishStop},
		{Content: "", FinishReason: core.FinishStop},
		{Content: "", FinishReason: core.FinishStop},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != EmptyFinalResponseMessage {
		t.Errorf("content = %q, want the empty-final-response notice", res.FinalContent)
	}
	if res.StopReason != StopEmptyFinalResponse {
		t.Errorf("stop = %q, want %q", res.StopReason, StopEmptyFinalResponse)
	}
	if res.Error != EmptyFinalResponseMessage {
		t.Errorf("error = %q, want the notice", res.Error)
	}
	// 1 initial + 1 retry + 1 finalization retry = 3
	if p.callCount() != 3 {
		t.Errorf("calls = %d, want 3", p.callCount())
	}
	if len(res.Messages) != 2 {
		t.Fatalf("transcript has %d messages, want 2: %+v", len(res.Messages), res.Messages)
	}
	last := res.Messages[1]
	if last.Role != core.RoleAssistant || !last.Content.IsText() ||
		last.Content.Text != EmptyFinalResponseMessage {
		t.Errorf("the transcript must end with the notice, got %+v", last)
	}
}

// TestProviderErrorSurfacesMessage pins the reference wording: a transport
// failure becomes content "Error calling LLM: <detail>", which the runner then
// surfaces verbatim because it is non-blank
// (base.py:950 + runner.py:715).
func TestProviderErrorSurfacesMessage(t *testing.T) {
	p := &scriptedProvider{err: errors.New("connection refused")}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopError {
		t.Errorf("stop = %q", res.StopReason)
	}
	want := "Error calling LLM: connection refused"
	if res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
	if res.FinalContent != want {
		t.Errorf("final content = %q, want %q", res.FinalContent, want)
	}
	// The transcript must not end on a bare user turn.
	last := res.Messages[len(res.Messages)-1]
	if last.Role != core.RoleAssistant {
		t.Errorf("last role = %q, want assistant", last.Role)
	}
	if last.Content.Text != PersistedModelErrorPlaceholder {
		t.Errorf("placeholder = %q", last.Content.Text)
	}
}

// TestErrorResponseWithBlankContentUsesDefault covers the other branch: when a
// provider reports an error with no text, the configured message is used.
func TestErrorResponseWithBlankContentUsesDefault(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "", FinishReason: core.FinishError},
	}}
	res, _ := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if res.Error != DefaultError {
		t.Errorf("error = %q, want %q", res.Error, DefaultError)
	}
}

func TestArrearageMessage(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishError, ErrorType: "insufficient_quota"},
	}}
	res, _ := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if res.Error != ArrearageError {
		t.Errorf("error = %q", res.Error)
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &scriptedProvider{responses: []*core.Response{{Content: "x", FinishReason: core.FinishStop}}}
	res, err := NewRunner().Run(ctx, RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopCanceled {
		t.Errorf("stop = %q, want canceled", res.StopReason)
	}
	if p.callCount() != 0 {
		t.Error("provider called despite cancelled context")
	}
}

func TestUsageAccumulates(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{toolCall("c", "echo", `{}`)},
			Usage:     &core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{Content: "done", FinishReason: core.FinishStop,
			Usage: &core.Usage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27}},
	}}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	res, _ := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	if res.Usage == nil {
		t.Fatal("usage is nil")
	}
	if res.Usage.PromptTokens != 30 || res.Usage.CompletionTokens != 12 || res.Usage.TotalTokens != 42 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestStreamingAggregatesAndForwardsDeltas(t *testing.T) {
	p := &streamingProvider{scriptedProvider{responses: []*core.Response{
		{Content: "streamed answer", FinishReason: core.FinishStop},
	}}}
	var mu sync.Mutex
	var deltas []string
	hook := &recordingHook{onText: func(s string) {
		mu.Lock()
		deltas = append(deltas, s)
		mu.Unlock()
	}}

	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Hook:     hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "streamed answer" {
		t.Errorf("content = %q", res.FinalContent)
	}
	mu.Lock()
	joined := strings.Join(deltas, "")
	n := len(deltas)
	mu.Unlock()
	if joined != "streamed answer" {
		t.Errorf("deltas joined = %q", joined)
	}
	if n < 2 {
		t.Errorf("expected multiple deltas, got %d", n)
	}
}

// TestStreamingToolCallsAggregated covers providers that stream tool-call
// fragments rather than returning them whole.
func TestStreamingToolCallsAggregated(t *testing.T) {
	p := &streamingProvider{scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{toolCall("c1", "echo", `{"v":"streamed"}`)}},
		{Content: "done", FinishReason: core.FinishStop},
	}}}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "done" {
		t.Errorf("content = %q", res.FinalContent)
	}
	var toolMsg string
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			toolMsg = m.Content.Text
		}
	}
	if toolMsg != "echo:streamed" {
		t.Errorf("streamed tool args not aggregated: %q", toolMsg)
	}
}

type recordingHook struct {
	NopHook
	onText func(string)
}

func (h *recordingHook) OnTextDelta(ctx context.Context, s string) {
	if h.onText != nil {
		h.onText(s)
	}
}

// TestSequentialByDefault verifies the reference default: tools run in order,
// one at a time, unless ConcurrentTools is set.
func TestSequentialByDefault(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{
			toolCall("1", "echo", `{"v":"a"}`),
			toolCall("2", "echo", `{"v":"b"}`),
		}},
		{Content: "done", FinishReason: core.FinishStop},
	}}

	res, _ := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	var got []string
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			got = append(got, m.Content.Text)
		}
	}
	if len(got) != 2 || got[0] != "echo:a" || got[1] != "echo:b" {
		t.Errorf("sequential order wrong: %v", got)
	}
}

// TestParallelPreservesCallOrder verifies results are ordered by call index
// even when a later call finishes first.
func TestParallelPreservesCallOrder(t *testing.T) {
	reg := tools.NewRegistry()
	slow := &echoTool{name: "slow", delay: 60 * time.Millisecond}
	fast := &echoTool{name: "fast"}
	reg.Register(slow)
	reg.Register(fast)

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{
			toolCall("1", "slow", `{"v":"s"}`),
			toolCall("2", "fast", `{"v":"f"}`),
		}},
		{Content: "done", FinishReason: core.FinishStop},
	}}

	start := time.Now()
	res, _ := NewRunner().Run(context.Background(), RunSpec{
		Messages:        []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider:        p,
		Tools:           reg,
		ConcurrentTools: true,
	})
	elapsed := time.Since(start)

	var got []string
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			got = append(got, m.Content.Text)
		}
	}
	if len(got) != 2 || got[0] != "slow:s" || got[1] != "fast:f" {
		t.Errorf("parallel result order wrong: %v", got)
	}
	// If run in parallel, total time should be well under 2x the slow delay.
	if elapsed > 110*time.Millisecond {
		t.Errorf("tools appear sequential: took %v", elapsed)
	}
}

// TestUnsafeToolForcesSequential verifies that one non-concurrency-safe tool in
// the batch disables parallelism for the whole batch.
func TestUnsafeToolForcesSequential(t *testing.T) {
	reg := tools.NewRegistry()
	slow := &echoTool{name: "slow", delay: 60 * time.Millisecond}
	writer := &writeTool{}
	reg.Register(slow)
	reg.Register(writer)

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{
			toolCall("1", "slow", `{"v":"s"}`),
			toolCall("2", "write_thing", `{}`),
		}},
		{Content: "done", FinishReason: core.FinishStop},
	}}

	start := time.Now()
	_, _ = NewRunner().Run(context.Background(), RunSpec{
		Messages:        []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider:        p,
		Tools:           reg,
		ConcurrentTools: true,
	})
	elapsed := time.Since(start)
	if elapsed < 55*time.Millisecond {
		t.Errorf("unsafe tool should force sequential; took %v", elapsed)
	}
	if writer.calls != 1 {
		t.Errorf("writer calls = %d", writer.calls)
	}
}

func TestMaxParallelToolsBoundsConcurrency(t *testing.T) {
	reg := tools.NewRegistry()
	var concurrent atomic.Int64
	var peak atomic.Int64
	track := &trackingTool{peak: &peak, current: &concurrent}
	reg.Register(track)

	calls := make([]core.ToolCall, 6)
	for i := range calls {
		calls[i] = toolCall(fmt.Sprintf("c%d", i), "track", `{}`)
	}
	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: calls},
		{Content: "done", FinishReason: core.FinishStop},
	}}

	_, _ = NewRunner().Run(context.Background(), RunSpec{
		Messages:         []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider:         p,
		Tools:            reg,
		ConcurrentTools:  true,
		MaxParallelTools: 2,
	})
	if got := peak.Load(); got > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", got)
	}
}

type trackingTool struct {
	tools.ReadOnlyBase
	peak    *atomic.Int64
	current *atomic.Int64
}

func (t *trackingTool) Name() string                { return "track" }
func (t *trackingTool) Description() string         { return "tracks concurrency" }
func (t *trackingTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *trackingTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	cur := t.current.Add(1)
	for {
		peak := t.peak.Load()
		if cur <= peak || t.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	t.current.Add(-1)
	return tools.OK("ok"), nil
}

func TestContextCancellationDuringTool(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo", delay: 2 * time.Second})

	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{toolCall("1", "echo", `{}`)}},
		{Content: "done", FinishReason: core.FinishStop},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := NewRunner().Run(ctx, RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > time.Second {
		t.Errorf("cancellation did not interrupt tool: %v", elapsed)
	}
	// The run should have stopped, not silently produced a final answer.
	if res.StopReason == StopCompleted && res.FinalContent == "done" {
		t.Error("run continued past cancellation")
	}
}

func TestCallerMessagesNotMutated(t *testing.T) {
	original := []core.Message{*core.NewMessage(core.RoleUser, "hi")}
	p := &scriptedProvider{responses: []*core.Response{{Content: "x", FinishReason: core.FinishStop}}}

	_, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: original,
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 {
		t.Errorf("caller slice was mutated: len=%d", len(original))
	}
}

func TestToolsAdvertisedToProvider(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})
	reg.Register(&writeTool{})

	p := &scriptedProvider{responses: []*core.Response{{Content: "x", FinishReason: core.FinishStop}}}
	_, _ = NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider: p,
		Tools:    reg,
	})

	if len(p.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	got := p.requests[0].Tools
	if len(got) != 2 {
		t.Fatalf("advertised %d tools, want 2", len(got))
	}
	// Registration order must be preserved for stable prompt prefix caching.
	if got[0].Name != "echo" || got[1].Name != "write_thing" {
		t.Errorf("tool order = %s, %s", got[0].Name, got[1].Name)
	}
}

func TestNoToolsAdvertisedWhenRegistryEmpty(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{{Content: "x", FinishReason: core.FinishStop}}}
	_, _ = NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider: p,
		Tools:    tools.NewRegistry(),
	})
	if len(p.requests[0].Tools) != 0 {
		t.Errorf("expected no tools, got %d", len(p.requests[0].Tools))
	}
}

func TestTruncateTextRuneSafety(t *testing.T) {
	// Multi-byte runes must not be split. TruncateText is the reference's
	// truncate_text (helpers.py:400), used when there is no workspace to
	// offload into.
	s := strings.Repeat("é", 100) // 2 bytes each
	got := TruncateText(s, 50)
	if !strings.Contains(got, "truncated") {
		t.Errorf("missing marker: %q", got)
	}
	// The reference appends its suffix AFTER cutting to the limit, so the
	// result is max_chars + len(suffix) characters — verified by running
	// truncate_text(5000 chars, 200) and observing 216 characters.
	if len([]rune(got)) > 50+len(textutil.TruncatedSuffix) {
		t.Errorf("result exceeds limit+suffix: %d", len([]rune(got)))
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Errorf("invalid rune introduced by truncation: %q", got)
		}
	}
}

func TestTruncateTextNoOpWhenUnderLimit(t *testing.T) {
	s := "short"
	if got := TruncateText(s, 100); got != s {
		t.Errorf("got %q", got)
	}
	if got := TruncateText(s, 0); got != s {
		t.Errorf("limit 0 should disable truncation, got %q", got)
	}
}
