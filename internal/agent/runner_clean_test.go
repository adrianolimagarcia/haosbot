package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// These tests cover `clean` — the value the reference computes as
// `hook.finalize_content(context, response.content)` at runner.py:588, :629 and
// :1214.
//
// The reference's turn hook chain ALWAYS begins with an AgentProgressHook
// (build_agent_turn_hook constructs it unconditionally, turn_hooks.py:45), and
// that hook's finalize_content is `strip_think(content) or None`
// (progress_hook.py:46-49, :180-181). So `clean` is the Python-stripped,
// think-tag-free response text, and empty means None. This port previously used
// strings.TrimSpace(resp.Content) at some sites and the raw content at others;
// both were wrong, and the second one leaked inline thinking tags to the user.

// ---------------------------------------------------------------------------
// The empty-content retry eligibility table (runner.py:588-593)
// ---------------------------------------------------------------------------

// TestEmptyRetryFinishReasonTable is the regression test for the missing
// finish_reason guard. Each row is a combination the reference and the port
// used to disagree on; the `length` row was the damaging one, because a blank
// truncated response consumed an empty-retry slot and re-requested the model
// WITHOUT the length-recovery continuation message.
//
// Every expectation below was read off runner.py:588-593 together with the
// control flow that reaches it, and the decision predicate itself is compared
// against the Python truth table in compat (TestRunnerCleanEmptyRetryTruthTable).
func TestEmptyRetryFinishReasonTable(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	toolCalls := []core.ToolCall{toolCall("c1", "echo", `{"v":"x"}`)}

	cases := []struct {
		name         string
		first        *core.Response
		wantCalls    int
		wantFinal    string
		wantStop     StopReason
		wantToolRan  bool
		wantRecovery bool // the 2nd request must carry the length-recovery prompt
		// wantNotice asserts the PERSISTED TRANSCRIPT ends with the
		// empty-final-response notice, which is what runner.py:741 writes
		// through _append_final_message. The returned content alone cannot
		// distinguish that from appending the blank response.
		wantNotice bool
		why        string
	}{
		{
			name:         "length_blank_no_tools_does_length_recovery",
			first:        &core.Response{Content: "", FinishReason: core.FinishLength},
			wantCalls:    2,
			wantFinal:    "fallback",
			wantStop:     StopCompleted,
			wantRecovery: true,
			why:          "finish_reason 'length' is excluded from the empty-retry check, so the length-recovery path runs instead",
		},
		{
			name:       "refusal_blank_no_tools_does_not_retry",
			first:      &core.Response{Content: "", FinishReason: core.FinishRefusal},
			wantCalls:  1,
			wantFinal:  EmptyFinalResponseMessage,
			wantStop:   StopEmptyFinalResponse,
			wantNotice: true,
			why:        "refusal is a deliberate provider outcome; a retry would only repeat it",
		},
		{
			name:       "content_filter_blank_no_tools_does_not_retry",
			first:      &core.Response{Content: "", FinishReason: core.FinishContentFilter},
			wantCalls:  1,
			wantFinal:  EmptyFinalResponseMessage,
			wantStop:   StopEmptyFinalResponse,
			wantNotice: true,
			why:        "content_filter is excluded for the same reason as refusal",
		},
		{
			name:      "error_blank_no_tools_returns_early",
			first:     &core.Response{Content: "", FinishReason: core.FinishError},
			wantCalls: 1,
			wantFinal: DefaultError,
			wantStop:  StopError,
			why:       "the error branch returns before the retry check",
		},
		{
			name:      "stop_blank_no_tools_retries",
			first:     &core.Response{Content: "", FinishReason: core.FinishStop},
			wantCalls: 2,
			wantFinal: "fallback",
			wantStop:  StopCompleted,
			why:       "a blank 'stop' response is the case the empty-retry feature exists for",
		},
		{
			name:        "stop_blank_with_tools_executes_them",
			first:       &core.Response{Content: "", FinishReason: core.FinishStop, ToolCalls: toolCalls},
			wantCalls:   2,
			wantFinal:   "fallback",
			wantStop:    StopCompleted,
			wantToolRan: true,
			why:         "the tool branch runs before the retry check and consumes the response",
		},
		{
			name:       "refusal_blank_with_tools_does_not_retry_or_execute",
			first:      &core.Response{Content: "", FinishReason: core.FinishRefusal, ToolCalls: toolCalls},
			wantCalls:  1,
			wantFinal:  EmptyFinalResponseMessage,
			wantStop:   StopEmptyFinalResponse,
			wantNotice: true,
			why:        "refusal blocks tool execution (should_execute_tools) and is excluded from retry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedProvider{responses: []*core.Response{tc.first}}
			res, err := NewRunner().Run(context.Background(), RunSpec{
				Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
				Provider: p,
				Tools:    reg,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := providerCalls(p); got != tc.wantCalls {
				t.Errorf("provider calls = %d, want %d (%s)", got, tc.wantCalls, tc.why)
			}
			if res.FinalContent != tc.wantFinal {
				t.Errorf("final content = %q, want %q", res.FinalContent, tc.wantFinal)
			}
			if res.StopReason != tc.wantStop {
				t.Errorf("stop reason = %q, want %q", res.StopReason, tc.wantStop)
			}
			if ran := toolRan(res); ran != tc.wantToolRan {
				t.Errorf("tool executed = %v, want %v", ran, tc.wantToolRan)
			}
			if tc.wantNotice {
				if n := len(res.Messages); n == 0 ||
					res.Messages[n-1].Role != core.RoleAssistant ||
					!res.Messages[n-1].Content.IsText() ||
					res.Messages[n-1].Content.Text != EmptyFinalResponseMessage {
					t.Errorf("the transcript must end with the empty-final-response notice "+
						"(runner.py:741), got %+v", res.Messages)
				}
				if res.Error != EmptyFinalResponseMessage {
					t.Errorf("error = %q, want the notice (runner.py:740 sets "+
						"context.error = final_content)", res.Error)
				}
			}
			if tc.wantRecovery {
				if len(p.requests) < 2 {
					t.Fatalf("only %d request(s) recorded", len(p.requests))
				}
				if !requestHasText(p.requests[1], LengthRecoveryPrompt) {
					t.Error("the follow-up request does not carry the length-recovery prompt: " +
						"the response was retried as an empty response instead of being continued")
				}
			} else if len(p.requests) > 1 && requestHasText(p.requests[1], LengthRecoveryPrompt) {
				t.Error("unexpected length-recovery prompt on the follow-up request")
			}
		})
	}
}

// TestEmptyRetryEligibleTruthTable exercises the predicate itself over every
// finish reason the port models, in all three blankness states, and asserts the
// runner's own condition agrees with the reference formula:
//
//	not should_execute_tools and finish_reason not in {error,length,refusal,content_filter}
//	    and is_blank_text(clean)
//
// (runner.py:588-593, with should_execute_tools from base.py:603.)
func TestEmptyRetryEligibleTruthTable(t *testing.T) {
	reasons := []core.FinishReason{
		core.FinishStop, core.FinishToolCalls, core.FinishFunctionCall,
		core.FinishLength, core.FinishContentFilter, core.FinishRefusal, core.FinishError,
	}
	excluded := map[core.FinishReason]bool{
		core.FinishError: true, core.FinishLength: true,
		core.FinishRefusal: true, core.FinishContentFilter: true,
	}
	// clean values: None and "" are the same thing after `or None`, so they are
	// both modelled by "".
	cleans := []string{"", "   ", "\u00a0", "\x1c", "x", "<think>t</think>"}

	checked := 0
	for _, fr := range reasons {
		for _, clean := range cleans {
			for _, hasCalls := range []bool{false, true} {
				resp := &core.Response{FinishReason: fr}
				if hasCalls {
					resp.ToolCalls = []core.ToolCall{toolCall("c", "echo", `{}`)}
				}
				// The runner's condition, spelled exactly as it appears in the
				// loop: the tool guard is the Go stand-in for "the tool branch
				// did not consume this response".
				got := !resp.HasToolCalls() && EmptyRetryEligible(fr, clean)

				// The reference's condition.
				want := !resp.ShouldExecuteTools() &&
					!excluded[fr] &&
					IsBlankText(clean)
				if got != want {
					t.Errorf("finish=%q clean=%q toolCalls=%v: got %v, reference %v",
						fr, clean, hasCalls, got, want)
				}
				checked++
			}
		}
	}
	if checked != len(reasons)*len(cleans)*2 {
		t.Fatalf("checked %d combinations, want %d", checked, len(reasons)*len(cleans)*2)
	}
	t.Logf("compared %d predicate combinations", checked)
}

// TestBlankTestIsPythonStrip pins the whitespace definition. U+001C..U+001F are
// Python whitespace but NOT Go unicode.IsSpace, so a strings.TrimSpace-based
// blank test answers differently for all four. NBSP U+00A0 is whitespace for
// BOTH, so it is included as a control rather than as a divergence.
//
// Reference values (utils/runtime.py:63, run through the venv):
//
//	is_blank_text("\x1c") is True, is_blank_text("\u00a0") is True,
//	is_blank_text("\u200b") is False.
func TestBlankTestIsPythonStrip(t *testing.T) {
	blank := []string{"", " ", "\t\n", "\u00a0", "\x1c", "\x1d", "\x1e", "\x1f",
		"\u3000", "\u2028", "\u0085", "\u1680"}
	for _, s := range blank {
		if !IsBlankText(s) {
			t.Errorf("IsBlankText(%q) = false, want true", s)
		}
	}
	notBlank := []string{"x", "\u200b", "\ufeff", "\u180e", "\u00a0x"}
	for _, s := range notBlank {
		if IsBlankText(s) {
			t.Errorf("IsBlankText(%q) = true, want false", s)
		}
	}

	// The divergence that motivates the whole helper: a lone U+001C is blank to
	// Python and not to strings.TrimSpace. Assert both sides so the test fails
	// if the two definitions ever converge or diverge further.
	if !IsBlankText("\x1c") {
		t.Error(`IsBlankText("\x1c") must be true (Python whitespace)`)
	}
	if strings.TrimSpace("\x1c") == "" {
		t.Error(`strings.TrimSpace("\x1c") unexpectedly reports blank; ` +
			"the trap this test guards no longer exists")
	}

	// End to end: a response whose content is only U+001C must take the
	// empty-content retry path, which strings.TrimSpace would have missed.
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "\x1c", FinishReason: core.FinishStop},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := providerCalls(p); got != 2 {
		t.Errorf("calls = %d, want 2 (U+001C content must count as blank)", got)
	}
	if res.FinalContent != "fallback" {
		t.Errorf("final content = %q, want fallback", res.FinalContent)
	}
}

// ---------------------------------------------------------------------------
// clean is delivered to the user
// ---------------------------------------------------------------------------

// TestFinalContentIsClean is the primary user-visible half of the bug: inline
// thinking tags the provider did not separate into a reasoning field must not
// reach the user. Reference: runner.py:785 (`final_content = clean`).
func TestFinalContentIsClean(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"think_block", "<think>secret reasoning</think>The answer.", "The answer."},
		{"orphan_close_tag", "</think>The answer.", "The answer."},
		{"trailing_whitespace", "The answer.  \n", "The answer."},
		{"leading_nbsp", "\u00a0The answer.", "The answer."},
		{"leading_u001c", "\x1cThe answer.", "The answer."},
		{"think_then_space", "  <think>x</think>  The answer.  ", "The answer."},
		{"plain", "The answer.", "The answer."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedProvider{responses: []*core.Response{
				{Content: tc.content, FinishReason: core.FinishStop},
			}}
			res, err := NewRunner().Run(context.Background(), RunSpec{
				Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
				Provider: p,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.FinalContent != tc.want {
				t.Errorf("FinalContent(%q) = %q, want %q",
					tc.content, res.FinalContent, tc.want)
			}
			// The Go answer must equal textutil.StripThink of the raw content,
			// which is what the reference's hook chain computes.
			if want := textutil.StripThink(tc.content); res.FinalContent != want {
				t.Errorf("FinalContent(%q) = %q, differs from StripThink = %q",
					tc.content, res.FinalContent, want)
			}
			if got := providerCalls(p); got != 1 {
				t.Errorf("calls = %d, want 1 (non-blank content must not retry)", got)
			}
		})
	}
}

// TestThinkOnlyContentCountsAsBlank is the counterpart: once strip_think has
// removed the thinking block there is no answer left, so the response is blank
// and the empty-content retry runs. The raw content was non-blank, so a
// TrimSpace-based blank test — and the port's old "never strip" behaviour —
// would have delivered the raw tags instead.
func TestThinkOnlyContentCountsAsBlank(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "<think>never closed", FinishReason: core.FinishStop},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := providerCalls(p); got != 2 {
		t.Errorf("calls = %d, want 2 (thinking-only content is blank)", got)
	}
	if res.FinalContent != "fallback" {
		t.Errorf("final content = %q, want fallback", res.FinalContent)
	}
}

// TestErrorBranchUsesClean covers runner.py:714-719. The reference is
// `final_content = clean or spec.error_message or _DEFAULT_ERROR_MESSAGE`, so a
// response whose content is only thinking tags or only Python whitespace must
// fall through to the configured message — strings.TrimSpace would have kept
// the tags as user-visible error text.
func TestErrorBranchUsesClean(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"think_only", "<think>boom</think>", DefaultError},
		{"think_only_then_ws", "  <think>boom</think>  ", DefaultError},
		{"nbsp_only", "\u00a0", DefaultError},
		{"u001c_only", "\x1c", DefaultError},
		{"blank", "", DefaultError},
		{"real_message", "  upstream exploded  ", "upstream exploded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedProvider{responses: []*core.Response{
				{Content: tc.content, FinishReason: core.FinishError},
			}}
			res, err := NewRunner().Run(context.Background(), RunSpec{
				Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
				Provider: p,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Error != tc.want {
				t.Errorf("Error(%q) = %q, want %q", tc.content, res.Error, tc.want)
			}
			if res.FinalContent != tc.want {
				t.Errorf("FinalContent(%q) = %q, want %q", tc.content, res.FinalContent, tc.want)
			}
		})
	}
}

// TestArrearageBeatsCleanText pins the ordering inside the error branch
// (runner.py:715): arrearage wins even when the response carries text.
func TestArrearageBeatsCleanText(t *testing.T) {
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "billing hard limit", FinishReason: core.FinishError,
			ErrorType: "insufficient_quota"},
	}}
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != ArrearageError {
		t.Errorf("Error = %q, want %q", res.Error, ArrearageError)
	}
}

// ---------------------------------------------------------------------------
// Length recovery uses clean for the segment and raw for the original
// ---------------------------------------------------------------------------

// TestLengthRecoverySplitsCleanAndOriginal covers runner.py:628-654: the
// segment is _restore_outer_whitespace(clean or "", original_content) — the
// STRIPPED text plus the boundary whitespace of the RAW one. Passing the raw
// content twice (as this port used to) doubles that boundary whitespace.
func TestLengthRecoverySplitsCleanAndOriginal(t *testing.T) {
	res, p := runScript(t, []*core.Response{
		truncated("  <think>why</think>alpha "),
		complete("beta"),
	})
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", p.calls)
	}
	// segment = _restore_outer_whitespace("alpha", "  <think>why</think>alpha ")
	//         = "  " + "alpha" + " " = "  alpha "
	// terminal = _restore_outer_whitespace("beta", "beta") = "beta"
	// final = ("  alpha " + "beta").strip() = "alpha beta"
	if res.FinalContent != "alpha beta" {
		t.Errorf("final content = %q, want %q", res.FinalContent, "alpha beta")
	}
}

// TestLengthRecoveryMessageUsesClean covers runner.py:654: the recovery prompt
// quotes the CLEANED segment, so the tail sent back to the model has no
// thinking tags and no boundary whitespace.
func TestLengthRecoveryMessageUsesClean(t *testing.T) {
	_, p := runScript(t, []*core.Response{
		truncated("<think>hidden</think>alpha beta "),
		complete("gamma"),
	})
	if len(p.requests) < 2 {
		t.Fatalf("only %d request(s)", len(p.requests))
	}
	var msg string
	for _, m := range p.requests[1].Messages {
		if m.Content.IsText() && strings.Contains(m.Content.Text, "<already_delivered_tail>") {
			msg = m.Content.Text
		}
	}
	if msg == "" {
		t.Fatal("no length-recovery message was sent")
	}
	if strings.Contains(msg, "<think>") || strings.Contains(msg, "hidden") {
		t.Errorf("the recovery prompt quotes the raw segment:\n%s", msg)
	}
	if !strings.Contains(msg, "<already_delivered_tail>\nalpha beta\n") {
		t.Errorf("the recovery prompt does not quote the cleaned tail:\n%s", msg)
	}
}

// ---------------------------------------------------------------------------
// Max-iterations salvage returns clean
// ---------------------------------------------------------------------------

// TestMaxIterationsSalvageReturnsClean covers runner.py:1211-1217, where the
// salvaged answer is `clean`, not the raw content.
func TestMaxIterationsSalvageReturnsClean(t *testing.T) {
	p := &salvageProvider{final: &core.Response{
		Content:      "  <think>internal</think>Here is what I completed so far.  ",
		FinishReason: core.FinishStop,
	}}
	res := runSalvage(t, p)

	if res.StopReason != StopMaxIterations {
		t.Errorf("stop reason = %q, want %q", res.StopReason, StopMaxIterations)
	}
	if res.FinalContent != "Here is what I completed so far." {
		t.Errorf("final content = %q, want the cleaned salvaged answer", res.FinalContent)
	}
}

// TestMaxIterationsSalvageRejectsBlankClean covers the other half: content that
// only looks non-blank to Go's TrimSpace (NBSP, or nothing but thinking tags)
// must still be rejected, so the static notice is used instead.
func TestMaxIterationsSalvageRejectsBlankClean(t *testing.T) {
	for _, content := range []string{"\u00a0", "\x1c", "<think>only thinking</think>", "   "} {
		p := &salvageProvider{final: &core.Response{Content: content, FinishReason: core.FinishStop}}
		res := runSalvage(t, p)

		if !strings.Contains(res.FinalContent, "maximum number of tool call iterations") {
			t.Errorf("content %q: expected the static fallback, got %q",
				content, res.FinalContent)
		}
	}
}

// ---------------------------------------------------------------------------
// errorResponseFromException
// ---------------------------------------------------------------------------

// TestErrorDetailFallbackUsesTypeName covers base.py:994:
// `detail = str(exc).strip() or type(exc).__name__`. The fallback is the
// exception's CLASS NAME; this port used the literal "unknown error".
//
// See goErrorTypeName for why the Go analogue is the dynamic type's
// unqualified name and where it necessarily differs from Python.
func TestErrorDetailFallbackUsesTypeName(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"plain_message", errors.New("connection refused"),
			"Error calling LLM: connection refused"},
		{"whitespace_only_message", errors.New("   "),
			"Error calling LLM: errorString"},
		{"nbsp_only_message", errors.New("\u00a0"),
			"Error calling LLM: errorString"},
		{"u001c_only_message", errors.New("\x1c"),
			"Error calling LLM: errorString"},
		{"empty_message", errors.New(""),
			"Error calling LLM: errorString"},
		{"custom_type", &cleanTestError{msg: ""},
			"Error calling LLM: cleanTestError"},
		{"wrapped_custom_type", &cleanTestError{msg: "  "},
			"Error calling LLM: cleanTestError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := errorResponseFromException(tc.err)
			if resp.Content != tc.want {
				t.Errorf("content = %q, want %q", resp.Content, tc.want)
			}
			if resp.FinishReason != core.FinishError {
				t.Errorf("finish reason = %q, want error", resp.FinishReason)
			}
		})
	}
}

// cleanTestError is an error whose message is not the type name, so the
// fallback branch can be exercised. Error has a value receiver so that both
// cleanTestError and *cleanTestError implement error.
type cleanTestError struct{ msg string }

func (e cleanTestError) Error() string { return e.msg }

// TestGoErrorTypeName pins the name-derivation rules, including the pointer
// unwrapping that makes *T and T agree.
func TestGoErrorTypeName(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"value_type", cleanTestError{}, "cleanTestError"},
		{"pointer_type", &cleanTestError{}, "cleanTestError"},
		{"errors_new", errors.New("x"), "errorString"},
		{"nil", nil, "unknown error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := goErrorTypeName(tc.err); got != tc.want {
				t.Errorf("goErrorTypeName(%T) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Streamed tool-call argument assembly
// ---------------------------------------------------------------------------

// TestStreamToolArgumentsUsePythonStrip covers the assembly of streamed
// tool-call arguments, whose reference counterpart is parse_tool_arguments
// (base.py:110-130): `stripped = arguments.strip()` and `if not stripped:
// return {}`. Python's strip, not Go's — so an argument payload consisting only
// of NBSP or U+001C is a no-arg call in the reference.
func TestStreamToolArgumentsUsePythonStrip(t *testing.T) {
	for _, args := range []string{"", "   ", "\u00a0", "\x1c", "\u3000", "\u2028"} {
		resp := finishStream(nil, "text", "", map[int]*streamPartial{
			0: {id: "c1", name: "echo", args: newArgBuffer(args)},
		}, []int{0})
		if len(resp.ToolCalls) != 1 {
			t.Fatalf("args %q: %d tool calls, want 1", args, len(resp.ToolCalls))
		}
		if got := string(resp.ToolCalls[0].Arguments); got != "{}" {
			t.Errorf("args %q: arguments = %q, want {}", args, got)
		}
	}

	// Non-blank arguments keep their content but lose Python boundary
	// whitespace, so the JSON stays parseable.
	resp := finishStream(nil, "", "", map[int]*streamPartial{
		0: {id: "c1", name: "echo", args: newArgBuffer("\u00a0{\"v\":\"x\"}\n")},
	}, []int{0})
	if got := string(resp.ToolCalls[0].Arguments); got != `{"v":"x"}` {
		t.Errorf("arguments = %q, want %s", got, `{"v":"x"}`)
	}
	var obj map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Arguments, &obj); err != nil {
		t.Errorf("arguments are not valid JSON: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Other `.strip()` sites in internal/agent
// ---------------------------------------------------------------------------

// TestPythonStripInAgentHelpers covers three more places where the reference
// calls Python's str.strip() and the port called strings.TrimSpace. They are
// not part of the `clean` chain, but they are the same trap: U+001C..U+001F are
// Python whitespace and not Go's.
//
// Each expectation was produced by running the reference:
//
//	ensure_nonempty_tool_result("t", "\x1c")  == "(t completed with no output)"
//	safe_filename("\x1ca.txt")                == "a.txt"
//
// and `text.strip() in PLACEHOLDER_TEXTS` (context_governance.py:783) is the
// third, exercised through StripPlaceholderAssistantMessages.
func TestPythonStripInAgentHelpers(t *testing.T) {
	marker := EmptyToolResultMessage("t")

	// ensure_nonempty_tool_result (runtime.py:53 and :58).
	for _, blank := range []string{"", "   ", "\u00a0", "\x1c", "\x1d\x1e\x1f"} {
		got := EnsureNonemptyContent("t", core.TextContent(blank))
		if got.Text != marker {
			t.Errorf("EnsureNonemptyContent(%q) = %q, want the marker %q",
				blank, got.Text, marker)
		}
	}
	if got := EnsureNonemptyContent("t", core.TextContent("x")); got.Text != "x" {
		t.Errorf("EnsureNonemptyContent(%q) = %q, want x", "x", got.Text)
	}
	// The block-list branch (runtime.py:58) uses the same definition.
	blocks := core.Content{Blocks: []core.ContentBlock{
		{Type: "text", Text: "\x1c"},
		{Type: "text", Text: "  "},
	}}
	if got := EnsureNonemptyContent("t", blocks); got.Text != marker {
		t.Errorf("EnsureNonemptyContent(blocks) = %q, want the marker %q", got.Text, marker)
	}
	// A non-text block is never empty on its own.
	mixed := core.Content{Blocks: []core.ContentBlock{
		{Type: "text", Text: "  "},
		{Type: "image", Text: ""},
	}}
	if got := EnsureNonemptyContent("t", mixed); got.Text == marker {
		t.Error("a block list containing an image must not be replaced by the marker")
	}

	// safe_filename (helpers.py:374-376).
	for _, tc := range []struct{ in, want string }{
		{"\x1ca.txt", "a.txt"},
		{"a.txt\x1f", "a.txt"},
		{"\u00a0a.txt\u00a0", "a.txt"},
		{" a.txt ", "a.txt"},
		{"a.txt", "a.txt"},
	} {
		if got := SafeFilename(tc.in); got != tc.want {
			t.Errorf("SafeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// strip_placeholder_assistant_messages (context_governance.py:783).
	const placeholder = "[Previous assistant message omitted.]"
	msgs := []core.Message{
		*core.NewMessage(core.RoleUser, "hi"),
		*core.NewMessage(core.RoleAssistant, "\x1c"+placeholder+"\x1c"),
		*core.NewMessage(core.RoleUser, "again"),
	}
	out := StripPlaceholderAssistantMessages(msgs)
	if len(out) != 2 {
		t.Errorf("placeholder wrapped in U+001C was not stripped: %d messages left", len(out))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// salvageProvider loops on tool calls until it sees the budget-exhausted
// prompt, then returns `final`. It is local to this file so the shared
// finalizationProvider in finalize_test.go stays untouched.
type salvageProvider struct {
	final *core.Response
}

func (p *salvageProvider) Name() string { return "salvage" }

func (p *salvageProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	for _, m := range req.Messages {
		if m.Content.IsText() && strings.Contains(m.Content.Text, BudgetExhaustedFinalizationPrompt) {
			return p.final, nil
		}
	}
	return &core.Response{
		FinishReason: core.FinishToolCalls,
		ToolCalls:    []core.ToolCall{toolCall("c1", "loop_tool", `{}`)},
	}, nil
}

// runSalvage drives a run to the iteration budget so the salvage path runs.
func runSalvage(t *testing.T, p *salvageProvider) *RunResult {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(&loopingTool{})
	res, err := NewRunner().Run(context.Background(), RunSpec{
		Messages:            []core.Message{*core.NewMessage(core.RoleUser, "do the thing")},
		Provider:            p,
		Tools:               reg,
		MaxIterations:       2,
		ContextWindowTokens: 200000,
		RetryDelays:         []float64{0},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// newArgBuffer builds the accumulated streamed-argument buffer.
func newArgBuffer(s string) strings.Builder {
	var b strings.Builder
	b.WriteString(s)
	return b
}

// providerCalls counts every request the scripted provider served. It is NOT
// scriptedProvider.callCount(): that counter is only incremented for scripted
// responses, so the "fallback" response it returns past the end of the script
// is not counted, and a test that wants to see a retry must not use it.
func providerCalls(p *scriptedProvider) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func toolRan(res *RunResult) bool {
	for _, m := range res.Messages {
		if m.Role == core.RoleTool {
			return true
		}
	}
	return false
}

func requestHasText(req provider.ChatRequest, needle string) bool {
	for _, m := range req.Messages {
		if m.Content.IsText() && strings.Contains(m.Content.Text, needle) {
			return true
		}
	}
	return false
}
