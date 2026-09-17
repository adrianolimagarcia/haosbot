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

// lengthProvider returns a scripted sequence of responses.
type lengthProvider struct {
	responses []*core.Response
	calls     int
	requests  []provider.ChatRequest
}

func (p *lengthProvider) Name() string { return "length" }

func (p *lengthProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.requests = append(p.requests, req)
	if p.calls >= len(p.responses) {
		// Anything beyond the script is a plain completion.
		return &core.Response{Content: "done", FinishReason: core.FinishStop}, nil
	}
	resp := p.responses[p.calls]
	p.calls++
	return resp, nil
}

func truncated(content string) *core.Response {
	return &core.Response{Content: content, FinishReason: core.FinishLength}
}

func complete(content string) *core.Response {
	return &core.Response{Content: content, FinishReason: core.FinishStop}
}

func runScript(t *testing.T, responses []*core.Response) (*RunResult, *lengthProvider) {
	t.Helper()
	p := &lengthProvider{responses: responses}
	reg := tools.NewRegistry()
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages:            []core.Message{*core.NewMessage(core.RoleUser, "write an essay")},
		Provider:            p,
		Tools:               reg,
		MaxIterations:       20,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, p
}

// TestLengthRecoveryStitchesSegments is the point of the feature: a response
// cut off by the output limit is continued, and the user receives one answer
// rather than only its first half.
func TestLengthRecoveryStitchesSegments(t *testing.T) {
	res, p := runScript(t, []*core.Response{
		truncated("The history of "),
		truncated("computing begins "),
		complete("with the abacus."),
	})

	if res.StopReason != StopCompleted {
		t.Errorf("stop reason = %q, want %q", res.StopReason, StopCompleted)
	}
	// Single spaces, not doubled. The earlier expectation here ("The history
	// of  computing begins  with the abacus.") was wrong: it rested on the
	// claim that "the base hook's finalize_content is the identity, so
	// clean == original". That is false for a turn — build_agent_turn_hook
	// always installs an AgentProgressHook first (turn_hooks.py:45) whose
	// finalize_content is `strip_think(content) or None`
	// (progress_hook.py:46-49, :180-181). So clean is the STRIPPED segment and
	// original is the RAW one, and _restore_outer_whitespace puts each
	// boundary character back exactly once instead of doubling it.
	//
	// Verified by executing the reference on these inputs:
	//   clean = strip_think(raw) or None
	//   part  = _restore_outer_whitespace(clean or '', raw)
	//   (''.join(parts) + part3).strip() == 'The history of computing begins with the abacus.'
	want := "The history of computing begins with the abacus."
	if res.FinalContent != want {
		t.Errorf("final content = %q, want %q", res.FinalContent, want)
	}
	if p.calls != 3 {
		t.Errorf("provider calls = %d, want 3", p.calls)
	}
}

// TestLengthRecoveryIsBounded verifies a model that never stops truncating
// cannot loop forever: after MaxLengthRecoveries the truncated text is
// accepted as it stands.
func TestLengthRecoveryIsBounded(t *testing.T) {
	responses := make([]*core.Response, 0, MaxLengthRecoveries+2)
	for i := 0; i < MaxLengthRecoveries+2; i++ {
		responses = append(responses, truncated("seg "))
	}
	res, p := runScript(t, responses)

	// One call per recovery attempt, plus the call whose answer is accepted.
	if p.calls != MaxLengthRecoveries+1 {
		t.Errorf("provider calls = %d, want %d", p.calls, MaxLengthRecoveries+1)
	}
	// The accepted answer is the joined recovered segments, not an error.
	// MaxLengthRecoveries recovered segments plus the terminal one that
	// exhausted the allowance, all concatenated. Each is
	// _restore_outer_whitespace(strip_think(raw), raw) for raw == "seg ", i.e.
	// "seg" with its single trailing space restored — the reference output for
	// these inputs, not the doubled "seg  " an identity finalize_content would
	// have produced.
	want := strings.TrimSpace(strings.Repeat("seg ", MaxLengthRecoveries+1))
	if res.FinalContent != want {
		t.Errorf("final content = %q, want %q", res.FinalContent, want)
	}
	// The answer must not grow past the bound no matter how much the provider
	// keeps truncating.
	if strings.Count(res.FinalContent, "seg") != MaxLengthRecoveries+1 {
		t.Errorf("segments = %d, want %d",
			strings.Count(res.FinalContent, "seg"), MaxLengthRecoveries+1)
	}
}

// TestLengthRecoveryMessageCarriesTail verifies the model is told where to
// resume. Without the quoted tail it tends to restart from the beginning.
func TestLengthRecoveryMessageCarriesTail(t *testing.T) {
	_, p := runScript(t, []*core.Response{
		truncated("alpha beta gamma"),
		complete(" delta"),
	})

	if len(p.requests) < 2 {
		t.Fatalf("only %d requests", len(p.requests))
	}
	var found string
	for _, m := range p.requests[1].Messages {
		if m.Content.IsText() && strings.Contains(m.Content.Text, "<already_delivered_tail>") {
			found = m.Content.Text
		}
	}
	if found == "" {
		t.Fatal("no length-recovery message was sent")
	}
	if !strings.Contains(found, "alpha beta gamma") {
		t.Errorf("the delivered tail is missing:\n%s", found)
	}
	if !strings.Contains(found, "Do not acknowledge this instruction") {
		t.Errorf("the prompt text is not the reference's:\n%s", found)
	}
	if !strings.HasSuffix(found, "Begin with the text that belongs immediately after this tail.") {
		t.Errorf("the prompt does not end with the resume instruction:\n%s", found)
	}
}

// TestLengthRecoveryTailIsRuneSafe guards against cutting a multi-byte
// character in half, which would send invalid UTF-8 to the provider.
func TestLengthRecoveryTailIsRuneSafe(t *testing.T) {
	// 100 multi-byte characters, well over the 64-rune tail limit.
	content := strings.Repeat("日", 100)
	msg := BuildLengthRecoveryMessage(content)

	if !strings.Contains(msg.Content.Text, strings.Repeat("日", LengthRecoveryTailChars)) {
		t.Error("the tail was not preserved as whole characters")
	}
	// A byte-sliced implementation produces invalid UTF-8.
	for i, r := range msg.Content.Text {
		if r == '\uFFFD' {
			t.Fatalf("invalid UTF-8 introduced at byte %d", i)
		}
	}
}

// TestLengthRecoveryTailIsClamped verifies a short answer quotes all of itself
// rather than being padded.
func TestLengthRecoveryTailIsClamped(t *testing.T) {
	msg := BuildLengthRecoveryMessage("short")
	if !strings.Contains(msg.Content.Text, "\nshort\n") {
		t.Errorf("short content was not quoted whole:\n%s", msg.Content.Text)
	}
}

// TestLengthRecoveryChainBrokenByToolWork verifies the chain is cleared when a
// tool runs: the user is no longer reading the same message, so stitching would
// produce a nonsensical answer.
func TestLengthRecoveryChainBrokenByToolWork(t *testing.T) {
	p := &lengthProvider{responses: []*core.Response{
		truncated("partial answer"),
		// Tool work happens here, ending the chain.
		{FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}}},
		complete("a fresh answer"),
	}}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages:            []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider:            p,
		Tools:               reg,
		MaxIterations:       20,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalContent != "a fresh answer" {
		t.Errorf("final content = %q, want the post-tool answer alone", res.FinalContent)
	}
}

// TestRestoreOuterWhitespace pins the boundary-whitespace repair directly,
// including the empty-original guard.
func TestRestoreOuterWhitespace(t *testing.T) {
	// Expected values were produced by running the reference function
	// (runner.py:77) on the same inputs. Note that passing content == original
	// doubles boundary whitespace, which is what the reference does too.
	cases := []struct {
		content, original, want string
	}{
		{"The history of ", "The history of ", "The history of  "},
		{"b", "  b  ", "  b  "},
		{"b", "b", "b"},
		{" b ", " b ", "  b  "},
		{"b", "", "b"}, // empty original: nothing to restore
		{"x", "\n x \n", "\n x \n"},

		// Non-ASCII boundary whitespace. The first version of this function
		// used the six ASCII characters as the cutset, which silently DROPPED
		// every one of these characters instead of restoring them. The
		// expectations below are the reference's own output.
		{"X", "\u00a0hi", "\u00a0X"},             // NO-BREAK SPACE
		{"X", "hi\u00a0", "X\u00a0"},             // trailing NO-BREAK SPACE
		{"X", "\u2003hi\u2003", "\u2003X\u2003"}, // EM SPACE, both sides
		{"X", "\x1chi", "\x1cX"},                 // U+001C: Python-only, Go unicode.IsSpace says false
		{"X", "\u3000hi", "\u3000X"},             // IDEOGRAPHIC SPACE
		{"X", "\u0085hi", "\u0085X"},             // NEL
		{"X", "\u2028hi", "\u2028X"},             // LINE SEPARATOR
		{"X", "\u202fhi", "\u202fX"},             // NARROW NO-BREAK SPACE
		{"X", "\u205fhi", "\u205fX"},             // MEDIUM MATHEMATICAL SPACE
		{"X", "\u1680hi", "\u1680X"},             // OGHAM SPACE MARK
		{"X", "\u2000hi", "\u2000X"},             // EN QUAD
		{"X", "\u200ahi", "\u200aX"},             // HAIR SPACE
		{"X", "\u00a0", "\u00a0X\u00a0"},         // all-whitespace still doubles, non-ASCII included
		{"X", "hi\u200b", "X"},                   // U+200B is NOT Python whitespace: nothing restored
	}
	for _, tc := range cases {
		if got := RestoreOuterWhitespace(tc.content, tc.original); got != tc.want {
			t.Errorf("RestoreOuterWhitespace(%q, %q) = %q, want %q",
				tc.content, tc.original, got, tc.want)
		}
	}
}
