package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// loopingTool always succeeds and always calls itself, so the run can only end
// by exhausting the iteration budget.
type loopingTool struct{ tools.ReadOnlyBase }

func (t *loopingTool) Name() string                { return "loop_tool" }
func (t *loopingTool) Description() string         { return "never finishes" }
func (t *loopingTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *loopingTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Content: "still working"}, nil
}

// finalizationProvider returns tool calls until it sees the budget-exhausted
// prompt, then behaves according to the scripted final behaviour.
type finalizationProvider struct {
	// finalKind selects what the post-budget call returns.
	finalKind string // "answer", "toolcall", "error", "blank"
	// requests records every request so the test can inspect the tool list.
	requests []provider.ChatRequest
}

func (p *finalizationProvider) Name() string { return "finalization" }

func (p *finalizationProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.requests = append(p.requests, req)

	sawBudgetPrompt := false
	for _, m := range req.Messages {
		if m.Content.IsText() && strings.Contains(m.Content.Text, "tool-call budget for this turn is exhausted") {
			sawBudgetPrompt = true
		}
	}
	if !sawBudgetPrompt {
		return &core.Response{
			FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{{
				ID: "c1", Name: "loop_tool", Arguments: json.RawMessage(`{}`),
			}},
		}, nil
	}

	switch p.finalKind {
	case "answer":
		return &core.Response{
			Content:      "Here is what I completed so far.",
			FinishReason: core.FinishStop,
		}, nil
	case "toolcall":
		// The model ignored "do not call tools".
		return &core.Response{
			FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{{
				ID: "c2", Name: "loop_tool", Arguments: json.RawMessage(`{}`),
			}},
		}, nil
	case "error":
		code := 500
		return &core.Response{FinishReason: core.FinishError, ErrorStatusCode: &code}, nil
	default: // blank
		return &core.Response{Content: "   ", FinishReason: core.FinishStop}, nil
	}
}

func runToBudget(t *testing.T, p *finalizationProvider, spec RunSpec) *RunResult {
	t.Helper()
	registry := tools.NewRegistry()
	registry.Register(&loopingTool{})

	spec.Messages = []core.Message{*core.NewMessage(core.RoleUser, "do the thing")}
	spec.Tools = registry
	spec.Provider = p
	spec.MaxIterations = 3
	spec.ContextWindowTokens = 200000
	// Keep the retry policy from sleeping through the scripted failure.
	spec.RetryDelays = []float64{0}

	res, err := NewRunner().Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestBudgetExhaustedFinalizationSalvagesAnswer is the point of the feature:
// the work done before the budget ran out is turned into a real answer instead
// of being thrown away for a static notice.
func TestBudgetExhaustedFinalizationSalvagesAnswer(t *testing.T) {
	p := &finalizationProvider{finalKind: "answer"}
	res := runToBudget(t, p, RunSpec{})

	if res.StopReason != StopMaxIterations {
		t.Errorf("stop reason = %q, want %q", res.StopReason, StopMaxIterations)
	}
	if res.FinalContent != "Here is what I completed so far." {
		t.Errorf("final content = %q, want the salvaged answer", res.FinalContent)
	}
	if strings.Contains(res.FinalContent, "maximum number of tool call iterations") {
		t.Error("fell back to the static notice despite a usable answer")
	}
}

// TestFinalizationRequestCarriesNoTools is the critical structural assertion:
// offering tools would let the model keep looping, defeating the budget.
func TestFinalizationRequestCarriesNoTools(t *testing.T) {
	p := &finalizationProvider{finalKind: "answer"}
	runToBudget(t, p, RunSpec{})

	last := p.requests[len(p.requests)-1]
	if len(last.Tools) != 0 {
		t.Errorf("finalization request carried %d tool schema(s), want 0", len(last.Tools))
	}
	// The earlier looping requests must still have had tools.
	if len(p.requests[0].Tools) == 0 {
		t.Error("the normal requests should carry tools")
	}
}

// TestFinalizationRejectsToolCalls verifies the guard against a model that
// ignores the instruction: executing those calls would contradict the budget.
func TestFinalizationRejectsToolCalls(t *testing.T) {
	p := &finalizationProvider{finalKind: "toolcall"}
	res := runToBudget(t, p, RunSpec{})

	if !strings.Contains(res.FinalContent, "maximum number of tool call iterations") {
		t.Errorf("expected the static fallback, got %q", res.FinalContent)
	}
}

// TestFinalizationRejectsError verifies a failed salvage attempt falls back
// rather than surfacing the failure.
func TestFinalizationRejectsError(t *testing.T) {
	p := &finalizationProvider{finalKind: "error"}
	res := runToBudget(t, p, RunSpec{})

	if !strings.Contains(res.FinalContent, "maximum number of tool call iterations") {
		t.Errorf("expected the static fallback, got %q", res.FinalContent)
	}
}

// TestFinalizationRejectsBlankContent verifies a whitespace-only answer is not
// presented as the result.
func TestFinalizationRejectsBlankContent(t *testing.T) {
	p := &finalizationProvider{finalKind: "blank"}
	res := runToBudget(t, p, RunSpec{})

	if !strings.Contains(res.FinalContent, "maximum number of tool call iterations") {
		t.Errorf("expected the static fallback, got %q", res.FinalContent)
	}
}

// TestFinalizationDisabledSkipsExtraRequest verifies the opt-out still works,
// which subagents rely on (subagent.py:435 passes False).
func TestFinalizationDisabledSkipsExtraRequest(t *testing.T) {
	disabled := false
	p := &finalizationProvider{finalKind: "answer"}
	res := runToBudget(t, p, RunSpec{FinalizeOnMaxIterations: &disabled})

	if !strings.Contains(res.FinalContent, "maximum number of tool call iterations") {
		t.Errorf("expected the static fallback, got %q", res.FinalContent)
	}
	// Exactly MaxIterations calls: no extra finalization request.
	if len(p.requests) != 3 {
		t.Errorf("provider called %d times, want 3 (no finalization request)", len(p.requests))
	}
	for _, req := range p.requests {
		for _, m := range req.Messages {
			if m.Content.IsText() && strings.Contains(m.Content.Text, "tool-call budget") {
				t.Error("the budget prompt was sent even though finalization was disabled")
			}
		}
	}
}

// TestBudgetPromptMatchesReferenceWording pins the prompt text. It is an
// instruction the model reads, and the reference deliberately forbids claiming
// completion without evidence.
func TestBudgetPromptMatchesReferenceWording(t *testing.T) {
	want := "The tool-call budget for this turn is exhausted. Based only on the " +
		"conversation and tool results above, provide a concise final response to " +
		"the user. Do not call or request tools. Do not claim the task is complete " +
		"unless the evidence above clearly shows it is complete. State what was " +
		"done, what remains, and the best next step if anything is incomplete."
	if BudgetExhaustedFinalizationPrompt != want {
		t.Errorf("prompt differs from utils/runtime.py:28:\n got: %q\nwant: %q",
			BudgetExhaustedFinalizationPrompt, want)
	}
}
