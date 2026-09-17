package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// fakeStreamProvider emits a streamed tool call with an id, then a final answer.
type fakeStreamProvider struct {
	call int
}

func (f *fakeStreamProvider) Name() string { return "fake-stream" }

func (f *fakeStreamProvider) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	return nil, nil
}

func (f *fakeStreamProvider) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan core.StreamEvent, error) {
	ch := make(chan core.StreamEvent, 8)
	go func() {
		defer close(ch)
		f.call++
		if f.call == 1 {
			ch <- core.StreamEvent{
				Kind:           core.StreamToolCall,
				Index:          0,
				ToolCallID:     "call_abc",
				ToolCallName:   "echo",
				ArgumentsDelta: `{"x":1}`,
			}
			ch <- core.StreamEvent{Kind: core.StreamDone}
			return
		}
		ch <- core.StreamEvent{Kind: core.StreamText, Text: "done"}
		ch <- core.StreamEvent{Kind: core.StreamDone}
	}()
	return ch, nil
}

type probeEchoTool struct{ tools.ReadOnlyBase }

func (probeEchoTool) Name() string                { return "probe_echo" }
func (probeEchoTool) Description() string         { return "echo" }
func (probeEchoTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (probeEchoTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	return tools.OK("echoed"), nil
}

// TestToolMessageCarriesCallID verifies that the tool result message sent back
// to the model carries the originating tool_call_id.
//
// This is not cosmetic: the OpenAI API requires every "tool" role message to
// reference the tool call it answers, and the reference always emits
// {"role","tool_call_id","name","content"} (runner.py:533-543). An empty id
// makes the follow-up request invalid on strict providers.
func TestToolMessageCarriesCallID(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(probeEchoTool{})

	runner := NewRunner()
	res, err := runner.Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: &fakeStreamProvider{},
		Tools:    reg,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var toolMsg *core.Message
	for i := range res.Messages {
		if res.Messages[i].Role == core.RoleTool {
			toolMsg = &res.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message produced")
	}

	raw, err := json.Marshal(toolMsg)
	if err != nil {
		t.Fatalf("marshal tool message: %v", err)
	}
	t.Logf("tool message on the wire: %s", raw)

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	idRaw, ok := decoded["tool_call_id"]
	if !ok {
		t.Fatalf("tool message has no tool_call_id key: %s", raw)
	}
	var id string
	if err := json.Unmarshal(idRaw, &id); err != nil {
		t.Fatalf("tool_call_id not a string: %s", idRaw)
	}
	if id != "call_abc" {
		t.Errorf("tool_call_id = %q, want %q", id, "call_abc")
	}

	// The assistant message must advertise the same id.
	var assistant *core.Message
	for i := range res.Messages {
		if res.Messages[i].Role == core.RoleAssistant && len(res.Messages[i].ToolCalls) > 0 {
			assistant = &res.Messages[i]
			break
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message with tool calls")
	}
	if got := assistant.ToolCalls[0].ID; got != "call_abc" {
		t.Errorf("assistant tool call id = %q, want %q", got, "call_abc")
	}
}
