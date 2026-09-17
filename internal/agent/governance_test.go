package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// capturingProvider records the exact messages it was asked to send, so a test
// can assert on what the model would actually receive.
type capturingProvider struct {
	seen   []core.Message
	calls  int
	reply  string
	status *int
}

func (p *capturingProvider) Name() string { return "capturing" }

func (p *capturingProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.calls++
	p.seen = append([]core.Message(nil), req.Messages...)
	if p.status != nil {
		return &core.Response{
			FinishReason:    core.FinishError,
			ErrorStatusCode: p.status,
		}, nil
	}
	return &core.Response{Content: p.reply, FinishReason: core.FinishStop}, nil
}

func assistantWithCall(id, name string) core.Message {
	m := *core.NewMessage(core.RoleAssistant, "")
	m.ToolCalls = []core.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}}
	return m
}

func toolResult(id, content string) core.Message {
	m := *core.NewMessage(core.RoleTool, content)
	m.ToolCallID = id
	m.Name = "read_file"
	return m
}

// TestRunnerDropsOrphanToolResultBeforeRequest is the end-to-end guard for the
// failure class that wedged sessions: a tool result with no declaring assistant
// turn makes upstream APIs reject the whole request.
func TestRunnerDropsOrphanToolResultBeforeRequest(t *testing.T) {
	p := &capturingProvider{reply: "ok"}
	runner := NewRunner()

	_, err := runner.Run(context.Background(), RunSpec{
		Messages: []core.Message{
			*core.NewMessage(core.RoleUser, "hi"),
			toolResult("ghost", "orphaned"),
		},
		Provider:            p,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range p.seen {
		if m.Role == core.RoleTool {
			t.Errorf("orphan tool result was sent to the provider: %+v", m)
		}
	}
	if len(p.seen) != 1 {
		t.Errorf("provider saw %d messages, want 1", len(p.seen))
	}
}

// TestRunnerBackfillsMissingToolResult covers the mirror defect: an assistant
// call whose result was lost (interrupted turn) is equally rejected.
func TestRunnerBackfillsMissingToolResult(t *testing.T) {
	p := &capturingProvider{reply: "ok"}
	runner := NewRunner()

	_, err := runner.Run(context.Background(), RunSpec{
		Messages: []core.Message{
			*core.NewMessage(core.RoleUser, "hi"),
			assistantWithCall("c1", "read_file"),
		},
		Provider:            p,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var filled *core.Message
	for i := range p.seen {
		if p.seen[i].Role == core.RoleTool {
			filled = &p.seen[i]
		}
	}
	if filled == nil {
		t.Fatal("no synthetic tool result was sent; the request would be rejected")
	}
	if filled.ToolCallID != "c1" {
		t.Errorf("synthetic result tool_call_id = %q, want c1", filled.ToolCallID)
	}
	if filled.Content.Text != BackfillContent {
		t.Errorf("synthetic result content = %q, want %q", filled.Content.Text, BackfillContent)
	}
}

// TestRunnerStripsMalformedToolCall covers a call whose function name was lost.
// Replaying it makes upstream APIs reject the request permanently.
func TestRunnerStripsMalformedToolCall(t *testing.T) {
	p := &capturingProvider{reply: "ok"}
	runner := NewRunner()

	_, err := runner.Run(context.Background(), RunSpec{
		Messages: []core.Message{
			*core.NewMessage(core.RoleUser, "hi"),
			assistantWithCall("bad", ""), // empty name: degenerate
			toolResult("bad", "result of the bad call"),
		},
		Provider:            p,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range p.seen {
		for _, tc := range m.ToolCalls {
			if tc.Name == "" {
				t.Errorf("malformed tool call (empty name) was sent: %+v", tc)
			}
		}
		if m.Role == core.RoleTool {
			t.Errorf("result of the stripped call was left dangling: %+v", m)
		}
	}
}

// TestRunnerCompactsWhenOverBudget verifies trimming happens once the request
// exceeds the window, and that the retained tail still starts at a user turn.
func TestRunnerCompactsWhenOverBudget(t *testing.T) {
	p := &capturingProvider{reply: "ok"}
	runner := NewRunner()

	big := strings.Repeat("x", 4000)
	var msgs []core.Message
	msgs = append(msgs, *core.NewMessage(core.RoleSystem, "system prompt"))
	for i := 0; i < 40; i++ {
		msgs = append(msgs, *core.NewMessage(core.RoleUser, big))
		msgs = append(msgs, *core.NewMessage(core.RoleAssistant, big))
	}
	msgs = append(msgs, *core.NewMessage(core.RoleUser, "final question"))

	_, err := runner.Run(context.Background(), RunSpec{
		Messages:            msgs,
		Provider:            p,
		ContextWindowTokens: 16384,
		MaxTokens:           4096,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(p.seen) >= len(msgs) {
		t.Errorf("no trimming happened: provider saw %d of %d messages", len(p.seen), len(msgs))
	}
	if len(p.seen) == 0 {
		t.Fatal("everything was trimmed away")
	}
	// The system prompt must survive: it is never trimmed.
	if p.seen[0].Role != core.RoleSystem {
		t.Errorf("first message role = %q, want system", p.seen[0].Role)
	}
	// The newest message must survive, since it is the actual question.
	last := p.seen[len(p.seen)-1]
	if last.Content.Text != "final question" {
		t.Errorf("last message = %q, want the final question", last.Content.Text)
	}
	// The retained tail must not begin with an orphaned tool result.
	if p.seen[len(p.seen)-1].Role == core.RoleTool {
		t.Error("retained tail ends with a tool result")
	}
	for i := 1; i < len(p.seen); i++ {
		if p.seen[i].Role == core.RoleTool {
			declared := false
			for j := 0; j < i; j++ {
				for _, tc := range p.seen[j].ToolCalls {
					if tc.ID == p.seen[i].ToolCallID {
						declared = true
					}
				}
			}
			if !declared {
				t.Errorf("retained tail has a tool result with no declaring call at index %d", i)
			}
		}
	}
}

// TestHistoryRepairIsUnconditional pins a distinction that is easy to get
// wrong: the reference repairs the model-facing copy ALWAYS, and only *fits*
// it to a budget when there is pressure.
//
// prepare_request (context_governance.py:602) calls prepare_messages_for_model
// before any context-window check; the window only gates fit_to_budget. A port
// that skipped repair when no window is configured would leave structurally
// invalid transcripts unrepaired — exactly the case where the provider rejects
// the whole request.
func TestHistoryRepairIsUnconditional(t *testing.T) {
	p := &capturingProvider{reply: "ok"}
	runner := NewRunner()

	_, err := runner.Run(context.Background(), RunSpec{
		Messages: []core.Message{
			*core.NewMessage(core.RoleUser, "hi"),
			toolResult("ghost", "orphaned"),
		},
		Provider: p,
		// ContextWindowTokens deliberately unset.
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seen) != 1 {
		t.Errorf("provider saw %d messages, want 1 (orphan must be repaired regardless)", len(p.seen))
	}
	for _, m := range p.seen {
		if m.Role == core.RoleTool {
			t.Errorf("orphan tool result survived with no context window configured: %+v", m)
		}
	}
}

// TestTrimmingRequiresWindow is the complement: without a declared window there
// is no budget to fit against, so nothing may be dropped.
func TestTrimmingRequiresWindow(t *testing.T) {
	p := &capturingProvider{reply: "ok"}
	runner := NewRunner()

	big := strings.Repeat("x", 4000)
	var msgs []core.Message
	for i := 0; i < 40; i++ {
		msgs = append(msgs, *core.NewMessage(core.RoleUser, big))
		msgs = append(msgs, *core.NewMessage(core.RoleAssistant, big))
	}

	_, err := runner.Run(context.Background(), RunSpec{
		Messages: msgs,
		Provider: p,
		// No ContextWindowTokens: nothing may be trimmed.
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seen) != len(msgs) {
		t.Errorf("provider saw %d messages, want all %d (no window means no trimming)",
			len(p.seen), len(msgs))
	}
}

// TestPrepareForModelLeavesCleanHistoryAlone guards against a repair that
// damages a healthy transcript.
func TestPrepareForModelLeavesCleanHistoryAlone(t *testing.T) {
	in := []core.Message{
		*core.NewMessage(core.RoleSystem, "sys"),
		*core.NewMessage(core.RoleUser, "hi"),
		assistantWithCall("c1", "read_file"),
		toolResult("c1", "contents"),
		*core.NewMessage(core.RoleAssistant, "done"),
	}
	out := PrepareForModel(in)
	if len(out) != len(in) {
		t.Fatalf("clean history was modified: %d -> %d messages", len(in), len(out))
	}
	for i := range in {
		if in[i].Role != out[i].Role {
			t.Errorf("message %d role changed: %q -> %q", i, in[i].Role, out[i].Role)
		}
	}
}

// TestInputBudgetZeroReservesNothing pins the subtle case: an explicit
// max_tokens of 0 means "no output reservation", not "use the default".
func TestInputBudgetZeroReservesNothing(t *testing.T) {
	zero := 0
	if got, want := InputBudget(200000, &zero), 200000-SnipSafetyBuffer; got != want {
		t.Errorf("InputBudget with explicit 0 = %d, want %d", got, want)
	}
	// nil means unset and falls back to the default output reservation.
	if got, want := InputBudget(200000, nil), 200000-defaultMaxOutputTokens-SnipSafetyBuffer; got != want {
		t.Errorf("InputBudget with nil = %d, want %d", got, want)
	}
}

// TestEnsureRequestFitsRaisesWhenHopeless verifies the terminal case: a single
// turn larger than the whole window cannot be trimmed into validity.
func TestEnsureRequestFitsRaisesWhenHopeless(t *testing.T) {
	msgs := []core.Message{*core.NewMessage(core.RoleUser, strings.Repeat("x", 50000))}
	maxTokens := 4096
	err := EnsureRequestFits(msgs, nil, 8192, &maxTokens, "cli:1")
	if err == nil {
		t.Fatal("expected ContextWindowExceededError")
	}
	var exceeded *ContextWindowExceededError
	if !asContextWindowExceeded(err, &exceeded) {
		t.Fatalf("error type = %T, want *ContextWindowExceededError", err)
	}
	if exceeded.SessionKey != "cli:1" {
		t.Errorf("session key = %q, want cli:1", exceeded.SessionKey)
	}
	if exceeded.InputBudget != 8192-4096-SnipSafetyBuffer {
		t.Errorf("input budget = %d, want %d", exceeded.InputBudget, 8192-4096-SnipSafetyBuffer)
	}
}

// asContextWindowExceeded is a small local helper so the test does not need to
// import errors solely for one assertion.
func asContextWindowExceeded(err error, target **ContextWindowExceededError) bool {
	e, ok := err.(*ContextWindowExceededError)
	if ok {
		*target = e
	}
	return ok
}
