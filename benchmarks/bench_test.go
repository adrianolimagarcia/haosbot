// Package benchmarks measures the performance and memory characteristics of
// nanobot-go's hot paths.
//
// Run with:
//
//	. scripts/goenv.sh && go test -bench=. -benchmem ./benchmarks/
//
// The package benchmarks the library directly rather than the binary, so
// results are reproducible without a running gateway or a live model endpoint.
// End-to-end binary RSS and startup are measured separately by
// scripts/bench.sh, which drives the real executable.
package benchmarks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// staticProvider always returns the same response, so benchmarks measure the
// framework rather than network variance.
type staticProvider struct {
	mu       sync.Mutex
	response *core.Response
}

func (p *staticProvider) Name() string { return "static" }

func (p *staticProvider) Chat(_ context.Context, _ provider.ChatRequest) (*core.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.response, nil
}

// sequenceProvider replays a fixed response sequence, then repeats the last one
// so a benchmark loop can run indefinitely.
type sequenceProvider struct {
	mu        sync.Mutex
	responses []*core.Response
	calls     int
}

func (p *sequenceProvider) Name() string { return "sequence" }

func (p *sequenceProvider) Chat(_ context.Context, _ provider.ChatRequest) (*core.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.responses[p.calls%len(p.responses)]
	p.calls++
	return r, nil
}

// echoTool is a trivial read-only tool.
type echoTool struct{ tools.ReadOnlyBase }

func (echoTool) Name() string                { return "echo" }
func (echoTool) Description() string         { return "echo" }
func (echoTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (echoTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.OK("ok"), nil
}

// namedTool is a tool with a distinct name, for registry-scale benchmarks.
type namedTool struct {
	tools.ReadOnlyBase
	name string
}

func (t namedTool) Name() string                { return t.name }
func (t namedTool) Description() string         { return "d" }
func (t namedTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t namedTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.OK("ok"), nil
}

// ---------------------------------------------------------------------------
// Prompt building
// ---------------------------------------------------------------------------

func BenchmarkPromptBuildSmall(b *testing.B) {
	builder := prompt.New(b.TempDir())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = builder.BuildSystemPrompt("cli", nil, "", true)
	}
}

func BenchmarkPromptBuildWithSkills(b *testing.B) {
	ws := b.TempDir()
	for i := 0; i < 50; i++ {
		dir := filepath.Join(ws, "skills", fmt.Sprintf("skill_%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# skill"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	builder := prompt.New(ws)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = builder.BuildSystemPrompt("cli", nil, "", true)
	}
}

// ---------------------------------------------------------------------------
// Message serialization
// ---------------------------------------------------------------------------

func BenchmarkMessageMarshal(b *testing.B) {
	msg := core.NewMessage(core.RoleAssistant, strings.Repeat("text ", 100))
	msg.Timestamp = "2026-09-16T02:38:34.123456"
	msg.ToolCalls = []core.ToolCall{{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/x"}`)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMessageUnmarshal(b *testing.B) {
	raw := []byte(`{"role":"assistant","content":"hello world","timestamp":"2026-09-16T02:38:34.123456","tool_calls":[{"id":"c1","name":"read_file","arguments":"{\"path\":\"/x\"}"}],"_hidden_history":false}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var m core.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Bus throughput
// ---------------------------------------------------------------------------

func BenchmarkBusPublishConsume(b *testing.B) {
	q := bus.New(bus.Options{})
	defer q.Close()
	ctx := context.Background()
	msg := core.InboundMessage{Channel: "cli", ChatID: "1", Content: "x"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := q.PublishInbound(ctx, msg); err != nil {
			b.Fatal(err)
		}
		if _, err := q.ConsumeInbound(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBusPublishParallel(b *testing.B) {
	q := bus.New(bus.Options{})
	ctx := context.Background()
	msg := core.InboundMessage{Channel: "cli", ChatID: "1", Content: "x"}

	// Drain concurrently so the queue does not grow without bound. The drainer
	// exits when Close() unblocks ConsumeInbound with ErrClosed — polling a
	// done channel would not work, because the goroutine is parked inside the
	// consume call and would never re-check it.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, err := q.ConsumeInbound(ctx); err != nil {
				return
			}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := q.PublishInbound(ctx, msg); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.StopTimer()
	q.Close()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Tool registry
// ---------------------------------------------------------------------------

func BenchmarkRegistryExecute(b *testing.B) {
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	call := core.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"a":1}`)}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := reg.Execute(ctx, call); res.IsError {
			b.Fatal(res.Content)
		}
	}
}

func BenchmarkRegistrySchemas(b *testing.B) {
	reg := tools.NewRegistry()
	for i := 0; i < 20; i++ {
		reg.Register(namedTool{name: fmt.Sprintf("tool_%d", i)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reg.Schemas()
	}
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

func BenchmarkRunnerSingleTurn(b *testing.B) {
	p := &staticProvider{response: &core.Response{
		Content: "a short answer", FinishReason: core.FinishStop}}
	runner := agent.NewRunner()
	ctx := context.Background()
	spec := agent.RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "hello")},
		Provider: p,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runner.Run(ctx, spec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunnerToolLoop(b *testing.B) {
	// One full tool round trip: tool call, then final answer.
	p := &sequenceProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{
			{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}}},
		{Content: "done", FinishReason: core.FinishStop},
	}}
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	runner := agent.NewRunner()
	ctx := context.Background()
	spec := agent.RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
		Tools:    reg,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runner.Run(ctx, spec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunnerLargeTranscript(b *testing.B) {
	p := &staticProvider{response: &core.Response{Content: "ok", FinishReason: core.FinishStop}}
	runner := agent.NewRunner()
	ctx := context.Background()

	msgs := make([]core.Message, 0, 200)
	for i := 0; i < 200; i++ {
		role := core.RoleUser
		if i%2 == 1 {
			role = core.RoleAssistant
		}
		msgs = append(msgs, *core.NewMessage(role, strings.Repeat("message body ", 20)))
	}
	spec := agent.RunSpec{Messages: msgs, Provider: p}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runner.Run(ctx, spec); err != nil {
			b.Fatal(err)
		}
	}
}
