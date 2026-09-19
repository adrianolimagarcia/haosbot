// Package agent implements the agent turn loop and the LLM/tool runner.
//
// This file ports the essential behavior of AgentRunner
// (upstream nanobot/agent/runner.py:141 at 1bb712d3).
//
// SCOPE NOTE — this is a faithful core, not yet a complete port. The following
// reference behaviors are deliberately NOT implemented and are tracked as
// roadmap items; they are listed here so their absence is explicit rather than
// silently mistaken for parity:
//
//   - injection callbacks (_MAX_INJECTIONS_PER_TURN/_MAX_INJECTION_CYCLES).
//     These drain a per-session pending queue that is fed by channels; with no
//     channels in this port there is no source of mid-turn messages, so the
//     callback has nothing to consume.
//   - provider-native compaction state (ProviderConversationState). The session
//     store already reads and writes the sidecar (session.go validProviderState);
//     what is missing is the controller that drives it during a request.
//   - checkpoint callbacks (CheckpointCallback).
//
// Implemented, in addition to the iteration loop: tool-call execution and
// reinjection, tool-result normalization and offload, empty-content retry,
// transient-error retry with the reference's backoff schedule, arrearage
// detection, cancellation, usage accumulation, context governance (repair,
// budget fitting and history snipping, in governance.go), budget-exhausted
// finalization, and length-recovery chains.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/observability"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// Reference limits, mirroring nanobot/agent/runner.py:65-74.
const (
	// DefaultError is the message shown when the model call fails.
	DefaultError = "Sorry, I encountered an error calling the AI model."
	// ArrearageError is shown when the provider reports quota exhaustion.
	ArrearageError = "The AI provider rejected the request because the API key is out of quota or the " +
		"account is in arrears. Please top up / check the billing status of your API key and try again."
	// maxEmptyRetries mirrors _MAX_EMPTY_RETRIES.
	maxEmptyRetries = 2
	// PersistedModelErrorPlaceholder mirrors _PERSISTED_MODEL_ERROR_PLACEHOLDER:
	// the assistant turn written to the transcript when the model call failed,
	// so the next turn still sees a well-formed assistant/user alternation.
	PersistedModelErrorPlaceholder = "[Assistant reply unavailable due to model error.]"
	// EmptyFinalResponseMessage mirrors EMPTY_FINAL_RESPONSE_MESSAGE
	// (utils/runtime.py:19-22). The reference substitutes it as final_content,
	// error AND the persisted assistant turn when the terminal model response is
	// blank (runner.py:737-745), and reports stop_reason
	// "empty_final_response".
	EmptyFinalResponseMessage = "I completed the tool steps but couldn't produce a final answer. " +
		"Please try again or narrow the task."
	// DefaultMaxToolIterations mirrors AgentDefaults.max_tool_iterations.
	DefaultMaxToolIterations = 200
	// DefaultMaxToolResultChars mirrors AgentDefaults.max_tool_result_chars.
	DefaultMaxToolResultChars = 16000
)

// StopReason explains why a run ended.
type StopReason string

const (
	// StopCompleted means the model produced a final answer.
	StopCompleted StopReason = "completed"
	// StopMaxIterations means the iteration budget was exhausted.
	StopMaxIterations StopReason = "max_iterations"
	// StopError means the run failed.
	StopError StopReason = "error"
	// StopCanceled means the caller cancelled the run.
	//
	// The value is "cancelled" with TWO Ls: the reference's only cancellation
	// spelling is `context.stop_reason = "cancelled"` (runner.py:321), and the
	// complete reference set of stop reasons is exactly {cancelled, completed,
	// empty_final_response, error, max_iterations}. The port previously used the
	// one-L "canceled", which diverged on the wire.
	StopCanceled StopReason = "cancelled"
	// StopEmptyFinalResponse means the terminal model response was blank
	// (runner.py:738-739).
	StopEmptyFinalResponse StopReason = "empty_final_response"
)

// Hook observes runner progress. All methods may be nil-checked by the runner;
// implement only what you need by embedding NopHook.
type Hook interface {
	// BeforeIteration is called before each model request.
	BeforeIteration(ctx context.Context, iteration int, messages []core.Message) error
	// OnTextDelta receives streamed assistant text.
	OnTextDelta(ctx context.Context, delta string)
	// OnReasoningDelta receives streamed reasoning text.
	OnReasoningDelta(ctx context.Context, delta string)
	// OnReasoningEnd marks the end of an in-flight reasoning stream.
	//
	// Mirrors emit_reasoning_end (hook.py:139): a hook that buffers
	// OnReasoningDelta chunks for in-place UI updates flushes and freezes the
	// rendered group here. The runner calls it after emitting a non-streamed
	// response's reasoning (runner.py:479), so a channel can finalize the
	// bubble instead of leaving it open forever.
	OnReasoningEnd(ctx context.Context)
	// OnToolStart is called before a tool executes.
	OnToolStart(ctx context.Context, call core.ToolCall)
	// OnToolEnd is called after a tool executes.
	OnToolEnd(ctx context.Context, call core.ToolCall, result core.ToolResult)
}

// NopHook is a Hook that does nothing. Embed it to implement a partial hook.
type NopHook struct{}

func (NopHook) BeforeIteration(context.Context, int, []core.Message) error { return nil }
func (NopHook) OnTextDelta(context.Context, string)                        {}
func (NopHook) OnReasoningDelta(context.Context, string)                   {}
func (NopHook) OnReasoningEnd(context.Context)                             {}
func (NopHook) OnToolStart(context.Context, core.ToolCall)                 {}
func (NopHook) OnToolEnd(context.Context, core.ToolCall, core.ToolResult)  {}

// RunSpec configures one agent execution.
// Mirrors nanobot/agent/runner.py:AgentRunSpec.
type RunSpec struct {
	// Messages is the initial conversation.
	Messages []core.Message
	// Tools is the tool registry. May be nil for a tool-free run.
	Tools *tools.Registry
	// Provider performs the model calls.
	Provider provider.Provider
	// Model overrides the provider's default model.
	Model string

	// RetryDelays is the transient-error backoff schedule. Nil selects the
	// reference schedule (1s, 2s, 4s => 4 attempts total). Tests set an
	// all-zero slice to exercise the policy without sleeping.
	RetryDelays []float64

	// Workspace is the agent's working directory. When set, oversized tool
	// results are offloaded there and the model receives a bounded reference
	// instead of the full payload. When empty the result is truncated in
	// place, which is what the reference does without a workspace.
	Workspace string

	// ContextWindowTokens enables context governance when non-zero. It is the
	// model's total context window; the input budget is derived from it by
	// subtracting the output reservation and a safety buffer.
	//
	// Zero disables governance entirely, matching the reference's
	// `if not config.context_window_tokens` guard. Callers that do not set it
	// get no history repair and no compaction.
	ContextWindowTokens int

	MaxIterations      int
	MaxToolResultChars int
	MaxTokens          int
	Temperature        float64
	ReasoningEffort    string

	// ConcurrentTools enables parallel tool execution.
	//
	// The AgentRunSpec dataclass defaults this to false, but AgentLoop
	// hard-codes concurrent_tools=True when it builds the spec
	// (loop.py:1179), so in the real runtime tools DO run in parallel. Only
	// tools declaring ConcurrencySafe are batched together; a tool that is not
	// concurrency-safe or that declares Exclusive runs strictly alone, and
	// results always stay in the model's call order.
	ConcurrentTools bool

	// MaxParallelTools bounds parallel tool goroutines. Zero means unlimited.
	MaxParallelTools int

	// SessionKey identifies the session, for diagnostics and tool routing.
	SessionKey string
	Channel    string
	ChatID     string
	Metadata   map[string]any
	// Hook observes progress.
	Hook Hook
	// Metrics receives allocation-light latency observations when configured.
	Metrics *observability.Registry
	// ErrorMessage overrides DefaultError.
	ErrorMessage string
	// MaxIterationsMessage overrides the default budget-exhausted message.
	MaxIterationsMessage string

	// FinalizeOnMaxIterations makes the runner ask the model for a final
	// answer WITHOUT tools before falling back to the static budget-exhausted
	// notice. Defaults to true, matching AgentRunSpec (runner.py:112).
	//
	// This is not cosmetic: the conversation at that point usually contains
	// enough evidence for a usable answer, and the reference salvages it
	// rather than discarding the whole turn.
	FinalizeOnMaxIterations *bool
}

// withDefaults returns a copy with zero values replaced by reference defaults.
func (s RunSpec) withDefaults() RunSpec {
	if s.MaxIterations <= 0 {
		s.MaxIterations = DefaultMaxToolIterations
	}
	if s.MaxToolResultChars <= 0 {
		s.MaxToolResultChars = DefaultMaxToolResultChars
	}
	if s.MaxTokens <= 0 {
		s.MaxTokens = 8192
	}
	if s.ErrorMessage == "" {
		s.ErrorMessage = DefaultError
	}
	return s
}

// RunResult is the outcome of an agent execution.
// Mirrors nanobot/agent/runner.py:AgentRunResult.
type RunResult struct {
	FinalContent string
	Messages     []core.Message
	ToolsUsed    []string
	Usage        *core.Usage
	StopReason   StopReason
	Error        string
}

// Runner executes agent runs.
type Runner struct{}

// NewRunner creates a runner.
func NewRunner() *Runner { return &Runner{} }

// Run executes the agent loop until a final answer, an iteration limit, or an
// error.
func (r *Runner) Run(ctx context.Context, spec RunSpec) (*RunResult, error) {
	spec = spec.withDefaults()
	hook := spec.Hook
	if hook == nil {
		hook = NopHook{}
	}

	// Copy so the caller's slice is not mutated by the run.
	messages := make([]core.Message, len(spec.Messages))
	copy(messages, spec.Messages)

	result := &RunResult{
		Messages:   messages,
		StopReason: StopCompleted,
	}

	var usage core.Usage
	var usageSeen bool
	seenTools := map[string]bool{}
	emptyRetries := 0
	// Segments from one uninterrupted length-recovery chain. Tool work or
	// injected user input starts a new logical answer and clears the chain,
	// because a continuation may only resume text the user is still reading.
	var lengthParts []string

	for iteration := 0; iteration < spec.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			result.StopReason = StopCanceled
			result.Error = err.Error()
			result.Messages = messages
			return result, nil
		}
		if err := hook.BeforeIteration(ctx, iteration, messages); err != nil {
			result.StopReason = StopError
			result.Error = err.Error()
			result.Messages = messages
			return result, nil
		}

		// streamedReasoning mirrors context.streamed_reasoning. It is declared
		// PER ITERATION, not per run, because the reference builds a fresh
		// AgentHookContext inside the loop (runner.py:435-440) and the field
		// defaults to False. Reasoning streamed in iteration 1 therefore does
		// not suppress a `reasoning_content` that arrives in iteration 2.
		streamedReasoning := false

		resp, err := r.requestModel(ctx, spec, messages, hook, &streamedReasoning)
		if err != nil {
			// Cancellation is a control-flow signal, not a model error.
			if ctx.Err() != nil {
				result.StopReason = StopCanceled
				result.Error = ctx.Err().Error()
				result.Messages = messages
				return result, nil
			}
			// A transport failure is converted into an error response so it
			// flows through the same reporting path as a provider-reported
			// error, mirroring _error_response_from_exception (base.py:950).
			resp = errorResponseFromException(err)
		}

		if resp.Usage != nil {
			usage.Add(resp.Usage)
			usageSeen = true
		}

		// original_content (runner.py:467, :628) is captured BEFORE
		// extract_reasoning replaces response.content just below, because
		// _restore_outer_whitespace later receives the RAW text: passing the
		// already-cleaned text there would restore the wrong boundary
		// whitespace (and, for the length-recovery path, double it).
		originalContent := resp.Content

		// `reasoning_text, cleaned_content = extract_reasoning(...)`
		// (runner.py:467). This runs on EVERY response, not only on streamed
		// ones. The three sources are, in priority order: a dedicated
		// reasoning_content, Anthropic thinking_blocks, and inline
		// <think>/<thinking>/<thought> blocks in the content itself. Only one
		// contributes, but inline tags are always scrubbed from the content so
		// they can never leak into the answer.
		reasoningText, cleanedContent := textutil.ExtractReasoning(
			&resp.ReasoningContent, resp.ThinkingBlocks, &resp.Content)
		// `response.content = cleaned_content` (runner.py:473). A nil cleaned
		// value is the reference storing None; "" stands in for it, which is
		// faithful because every later consumer either Python-truth-tests it or
		// runs it through IsBlankText / PyStrip.
		if cleanedContent != nil {
			resp.Content = *cleanedContent
		} else {
			resp.Content = ""
		}

		// runner.py:477-480. The reasoning a non-streaming response carried is
		// surfaced here, exactly once: `context.streamed_reasoning` suppresses
		// a re-emission when the deltas already went out through the streaming
		// path, and the flag is set afterwards so the NEXT response in this
		// iteration cannot emit twice either. The gate is Python truthiness on
		// reasoning_text, so an empty extraction (a whitespace-only
		// reasoning_content, or an empty <think></think> block) emits nothing.
		//
		// The end call is not cosmetic: emit_reasoning_end is what tells a
		// channel to flush and freeze the in-place reasoning bubble, and the
		// reference's own test asserts it fires once for a one-shot response
		// (tests/agent/test_runner_reasoning.py:344-377).
		if reasoningText != nil && *reasoningText != "" && !streamedReasoning {
			hook.OnReasoningDelta(ctx, *reasoningText)
			hook.OnReasoningEnd(ctx)
			streamedReasoning = true
		}

		// clean is `clean = hook.finalize_content(context, response.content)`
		// (runner.py:588, :629, :1214). It is NOT strings.TrimSpace of the raw
		// content: the reference's hook chain always begins with an
		// AgentProgressHook — build_agent_turn_hook installs it unconditionally
		// (turn_hooks.py:45) and it is the default hook for a turn — and that
		// hook's finalize_content is `strip_think(content) or None`
		// (progress_hook.py:46-49 and :180-181).
		//
		// Note that response.content is now extract_reasoning's cleaned text,
		// so this is the reference's own SECOND strip_think for a response that
		// carried content. The port previously stripped the raw content once;
		// the two agree wherever strip_think is idempotent, which is asserted
		// against the reference in compat/runner_clean_differential_test.go.
		//
		// Two consequences, both load-bearing:
		//
		//   - clean is Python-stripped, not Go-trimmed. The two disagree on
		//     U+001C..U+001F (FILE/GROUP/RECORD/UNIT SEPARATOR): Python's
		//     str.isspace() accepts them, Go's unicode.IsSpace does not.
		//     textutil.StripThink already ends in textutil.PyStrip, so it is
		//     the correct primitive. (NBSP U+00A0 is whitespace for BOTH, so it
		//     is not one of the divergences — verified in
		//     TestRunnerCleanBlankTextMatchesPython, which counts how many
		//     corpus entries actually separate the two definitions.)
		//   - `or None` collapses "" and None, and is_blank_text treats both as
		//     blank, so a Go string with "" standing in for None is faithful.
		//     Every later test is written as `clean == ""` / IsBlankText(clean).
		clean := textutil.StripThink(resp.Content)

		// A provider-level failure surfaces as a response, not a Go error, so
		// the retry policy can classify it (mirrors the Python contract).
		if resp.FinishReason == core.FinishError {
			// Reference order (runner.py:715): arrearage wins; otherwise prefer
			// the response's own non-blank text, then the configured message.
			// `clean or spec.error_message or _DEFAULT_ERROR_MESSAGE` is Python
			// TRUTHINESS, not blankness — but clean is already stripped, so
			// clean == "" is exactly "clean is falsy". The tail of the chain is
			// already in place: withDefaults sets ErrorMessage to DefaultError
			// (runner.py's _DEFAULT_ERROR_MESSAGE).
			msg := clean
			if isArrearage(resp) {
				msg = ArrearageError
			} else if msg == "" {
				msg = spec.ErrorMessage
			}
			messages = AppendModelErrorPlaceholder(messages)
			result.StopReason = StopError
			result.Error = msg
			result.FinalContent = msg
			result.Messages = messages
			return result, nil
		}

		// The reference drops tool calls with a missing or empty name inside
		// _request_model — BEFORE the retry check and the tool branch ever see
		// the response — and forces finish_reason to "stop" when that leaves
		// none (runner.py:1088-1111). The surviving calls are therefore what
		// both decisions below must be derived from; using resp.HasToolCalls()
		// here made a response whose only calls were degenerate look like
		// tool work to the retry check, so the blank response it really was
		// never retried.
		calls := validCalls(resp.ToolCalls)
		toolBranch := resp.ShouldExecuteTools() && len(calls) > 0

		// Empty-content retry: a model that returns neither content nor tool
		// calls has produced nothing usable; retry a bounded number of times
		// before accepting the empty answer (mirrors _MAX_EMPTY_RETRIES).
		//
		// Two parts of the reference condition are needed here
		// (runner.py:588-593):
		//
		//   - `finish_reason not in {"error","length","refusal","content_filter"}`.
		//     Without it a blank `length` response consumes an empty-retry slot
		//     and re-requests the model WITHOUT the length-recovery continuation
		//     message, so truncation recovery silently degrades into a plain
		//     retry. `error` is already returned above, but is kept in the
		//     predicate so the Go condition is the reference's, not a
		//     case-by-case reconstruction of it.
		//   - `is_blank_text(clean)`, i.e. the STRIPPED text, not the raw one.
		//
		// !toolBranch stands in for "the tool branch did not consume this
		// response": in the reference the `should_execute_tools` branch
		// (runner.py:~480-578) runs before line 588 and ends in `continue`, so
		// the check is only reached by a response the tool branch did not take.
		if !toolBranch && EmptyRetryEligible(resp.FinishReason, clean) {
			if emptyRetries < maxEmptyRetries {
				emptyRetries++
				continue
			}
		} else {
			emptyRetries = 0
		}

		// Length recovery. A response cut off by the output token limit is not
		// a finished answer, so continuing is preferred over delivering half
		// of one. Handled before the completion check below, which would
		// otherwise accept the truncated text as the final content.
		//
		// The recovered segments are joined at the end rather than returned
		// here, so the user sees one answer instead of several.
		if resp.FinishReason == core.FinishLength && len(lengthParts) < MaxLengthRecoveries {
			// The two arguments are DIFFERENT (runner.py:634): the content is
			// `clean or ""` — the stripped segment — and the original is the
			// RAW response.content, which is what supplies the boundary
			// whitespace back. Passing the raw content twice (as this port
			// used to) doubles every boundary whitespace character instead of
			// restoring it. `originalContent` is the pre-extract_reasoning
			// value: response.content has since been replaced by the cleaned
			// text (runner.py:473), and the reference restores from the
			// snapshot it took at runner.py:467.
			lengthParts = append(lengthParts, RestoreOuterWhitespace(clean, originalContent))
			// runner.py:646-653 persists `build_assistant_message(clean, ...)`
			// — the CLEANED segment, and no tool calls (finish_reason is
			// "length", so there are none to issue).
			messages = append(messages, assistantMessage(resp, clean, nil))
			// runner.py:654 quotes the cleaned segment back, not the raw one.
			messages = append(messages, BuildLengthRecoveryMessage(clean))
			continue
		}

		// The terminal section, mirroring runner.py:675-789. The assistant turn
		// is persisted at the END of it (runner.py:756-766), NOT before the
		// terminal decision: the blank branch below writes
		// EMPTY_FINAL_RESPONSE_MESSAGE through _append_final_message
		// (runner.py:741) instead of persisting the response it received, so
		// appending first leaves a blank assistant turn in the transcript that
		// the reference never writes.
		//
		// A response is terminal when the tool branch above does not consume
		// it. Two cases reach here (runner.py:482 and :1110):
		//
		//   - the finish reason is not tool-capable (refusal, content_filter,
		//     error, length), so should_execute_tools is false;
		//   - every tool call was degenerate and was dropped inside
		//     _request_model, which then forces finish_reason to "stop" so the
		//     response flows through this same terminal path.
		if !toolBranch {
			// runner.py:737-754 — the branch this port was missing. A terminal
			// response whose cleaned text is blank is not an answer: it is
			// replaced by the reference's fixed notice, reported as
			// stop_reason "empty_final_response", and — because the reference
			// sets context.error = final_content — surfaced as the run's error
			// as well. Note that any pending length-recovery segments are
			// DISCARDED here: the reference returns the notice, not
			// "".join(length_recovery_parts).
			//
			// IsBlankText is Python's strip (textutil.PyStrip), so "\x1c" and
			// NBSP are blank while strings.TrimSpace would keep them.
			if IsBlankText(clean) {
				result.FinalContent = EmptyFinalResponseMessage
				result.StopReason = StopEmptyFinalResponse
				result.Error = EmptyFinalResponseMessage
				// runner.py:741: the transcript records the NOTICE, replacing
				// a trailing assistant turn rather than stacking a second one.
				messages = AppendFinalMessage(messages, EmptyFinalResponseMessage)
				result.Messages = messages
				return result, nil
			}

			// runner.py:756-766 persists `build_assistant_message(clean, ...)`
			// with NO tool_calls argument. A response that carried calls under
			// a non-tool-capable finish reason is therefore stored as plain
			// text — the reference logs and ignores those calls
			// (runner.py:581-586) — and persisting them would replay an
			// unmatched tool call on the next request.
			messages = append(messages, assistantMessage(resp, clean, nil))

			if len(lengthParts) > 0 {
				// The chain ends here: stitch the recovered segments to the
				// terminal one and deliver the whole answer. Mirrors
				// runner.py:781-783, including the outer Python .strip() (NOT
				// strings.TrimSpace) and the clean/raw argument split.
				result.FinalContent = textutil.PyStrip(
					strings.Join(lengthParts, "") +
						RestoreOuterWhitespace(clean, originalContent))
			} else {
				// runner.py:785: `final_content = clean`. The raw content is
				// never delivered to the user — that is the whole point of
				// finalize_content, and it is why inline thinking blocks that
				// the provider did not separate into a reasoning field never
				// reach the user.
				result.FinalContent = clean
			}
			result.StopReason = StopCompleted
			break
		}

		for _, c := range calls {
			if !seenTools[c.Name] {
				seenTools[c.Name] = true
				result.ToolsUsed = append(result.ToolsUsed, c.Name)
			}
		}

		// Tool work starts a new logical answer, so a pending continuation can
		// no longer be stitched onto what the user is reading.
		lengthParts = nil

		// runner.py:487-497: the tool-call turn is persisted BEFORE the tools
		// run. Its content is the CLEANED text (`response.content or ""`, where
		// response.content was replaced by extract_reasoning's cleaned text at
		// runner.py:473) and its calls are the ones actually issued — the
		// malformed ones were already dropped, so replaying them would send the
		// provider a call with no matching tool result.
		messages = append(messages, assistantMessage(resp, clean, calls))

		results := r.executeTools(ctx, spec, calls, hook)
		for i, tr := range results {
			messages = append(messages, toolMessage(spec, tr, calls[i].Name))
		}

		if iteration == spec.MaxIterations-1 {
			result.StopReason = StopMaxIterations
			// Ask for a final answer before giving up. Mirrors
			// _try_finalize_after_max_iterations (runner.py:1163): the turn
			// usually holds enough evidence for a usable answer, and the
			// reference salvages it instead of discarding the work.
			terminalContent := maxIterationsMessage(spec)
			if content, ok := r.finalizeAfterMaxIterations(ctx, spec, messages, hook); ok {
				terminalContent = content
			}
			result.FinalContent = terminalContent
			// runner.py:823: `self._append_final_message(messages, terminal_content)`
			// — the other caller of the helper the blank branch uses. The value
			// written is terminal_content (the salvaged answer or the static
			// notice), NOT final_content, which additionally carries any
			// length-recovery prefix. Without this the transcript ends on a
			// tool result with no reply, which is the malformed alternation the
			// helper exists to prevent.
			messages = AppendFinalMessage(messages, terminalContent)
		}
	}

	if usageSeen {
		result.Usage = &usage
	}
	result.Messages = messages
	return result, nil
}

// requestModel performs one model call with the reference retry policy,
// delegating each attempt to requestModelOnce.
//
// Mirrors _chat_with_retry (base.py:1749-1910): with the default schedule of
// three delays there are FOUR attempts in total — attempts 1, 2 and 3 sleep
// 1s, 2s and 4s, and attempt 4 returns the failure. A provider-supplied
// Retry-After replaces the schedule and is padded by one second.
//
// A non-transient failure returns immediately: quota exhaustion and billing
// errors are permanent, and retrying them only delays the error.
//
// streamedReasoning is the caller's per-iteration `context.streamed_reasoning`.
// It is passed by POINTER because the reference's streaming callbacks set it on
// the shared AgentHookContext as deltas arrive (runner.py:975), so the flag is
// visible to the non-streamed extraction that runs after the same request. It
// is never cleared here, so it also survives the retry attempts inside one
// request, matching a context that outlives them.
func (r *Runner) requestModel(
	ctx context.Context,
	spec RunSpec,
	messages []core.Message,
	hook Hook,
	streamedReasoning *bool,
) (*core.Response, error) {
	delays := spec.RetryDelays
	if delays == nil {
		delays = provider.ChatRetryDelays
	}

	var last *core.Response
	for attempt := 1; ; attempt++ {
		resp, err := r.requestModelOnce(ctx, spec, messages, hook, streamedReasoning)
		if err != nil {
			return nil, err
		}
		// Success, or a failure that is not worth retrying.
		if resp.FinishReason != core.FinishError {
			return resp, nil
		}
		transient, retryAfter := provider.ClassifyResponse(resp)
		if !transient {
			return resp, nil
		}
		// Attempts are 1-based: give up once past the schedule.
		if attempt > len(delays) {
			return resp, nil
		}
		last = resp

		delay := provider.RetryDelay(attempt, retryAfter, delays)
		if delay > 0 {
			if err := SleepWithContext(ctx, time.Duration(delay*float64(time.Second))); err != nil {
				// Context cancelled during backoff: surface the last failure
				// rather than a spurious error, so the caller can persist it.
				return last, nil
			}
		}
	}
}

// requestModelOnce performs a single model call, streaming when the provider
// supports it so that text deltas reach the hook as they arrive.
func (r *Runner) requestModelOnce(
	ctx context.Context,
	spec RunSpec,
	messages []core.Message,
	hook Hook,
	streamedReasoning *bool,
) (*core.Response, error) {
	// Context governance, mirroring prepare_request (runner.py:877).
	//
	// The model-facing copy is always repaired; it is only *fitted* when the
	// request is actually at or over budget, because trimming history is
	// lossy and must not happen for a request that already fits.
	messages = PrepareForModel(messages)

	var toolSchemas []provider.ToolSchema
	if spec.Tools != nil && spec.Tools.Len() > 0 {
		toolSchemas = spec.Tools.Schemas()
	}
	if spec.ContextWindowTokens > 0 {
		if _, pressured := RequestPressure(messages, toolSchemas, spec.ContextWindowTokens, &spec.MaxTokens); pressured {
			fitted, err := FitToBudget(messages, toolSchemas, spec.ContextWindowTokens, &spec.MaxTokens, spec.SessionKey)
			if err != nil {
				return nil, err
			}
			messages = fitted
		}
	}

	req := provider.ChatRequest{
		Messages:        messages,
		Model:           spec.Model,
		MaxTokens:       spec.MaxTokens,
		Temperature:     spec.Temperature,
		ReasoningEffort: spec.ReasoningEffort,
		Tools:           toolSchemas,
	}

	sp, ok := spec.Provider.(provider.StreamingProvider)
	if !ok {
		started := time.Now()
		resp, err := spec.Provider.Chat(ctx, req)
		if spec.Metrics != nil {
			spec.Metrics.ObserveProviderTTFT(time.Since(started))
			spec.Metrics.ObserveProviderTotal(time.Since(started))
		}
		return resp, err
	}

	started := time.Now()
	stream, err := sp.ChatStream(ctx, req)
	if err != nil {
		if spec.Metrics != nil { spec.Metrics.ObserveProviderTotal(time.Since(started)) }
		return nil, err
	}
	var firstEvent func()
	if spec.Metrics != nil {
		firstEvent = func() { spec.Metrics.ObserveProviderTTFT(time.Since(started)) }
	}
	resp, err := consumeStream(ctx, stream, hook, streamedReasoning, firstEvent)
	if spec.Metrics != nil { spec.Metrics.ObserveProviderTotal(time.Since(started)) }
	return resp, err
}

// streamPartial accumulates the fragments of one streamed tool call.
// Providers emit the id and name on the first fragment for an index and only
// argument deltas afterwards.
type streamPartial struct {
	id   string
	name string
	args strings.Builder
}

// consumeStream aggregates stream events into a final response, forwarding
// deltas to the hook.
//
// A non-empty reasoning delta marks the caller's streamedReasoning flag, which
// is what stops the same reasoning being emitted a second time when the
// response's own thinking_blocks are inspected afterwards (runner.py:975 then
// :477). The reference sets the flag only for a NON-EMPTY delta, because
// _thinking returns early on an empty one; an empty event must not be able to
// suppress a later real reasoning payload.
//
// nativeReasoningOpen mirrors the reference's `native_reasoning_open`: it
// records whether an emit_reasoning_end is still owed for the group of deltas
// that just went out. The reference closes that group as soon as user-visible
// content starts (_stream -> _close_native_reasoning, runner.py:963) and again
// once the provider call returns (runner.py:1005), and on cancellation. Without
// the close, a channel that opens a reasoning bubble on the first delta would
// leave it open for the rest of the turn, with the answer streaming into it.
// The reference asserts this ordering directly in
// tests/agent/test_runner_reasoning.py:423-458 (reasoning, reasoning_end, then
// content) and :572-608 (close before propagating cancellation).
func consumeStream(ctx context.Context, stream <-chan core.StreamEvent, hook Hook, streamedReasoning *bool, firstEvent func()) (*core.Response, error) {
	var text strings.Builder
	var reasoning strings.Builder
	partials := map[int]*streamPartial{}
	var order []int
	var final *core.Response

	nativeReasoningOpen := false
	closeNativeReasoning := func() {
		if nativeReasoningOpen {
			nativeReasoningOpen = false
			hook.OnReasoningEnd(ctx)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// The reference settles the reasoning group before the
			// CancelledError propagates.
			closeNativeReasoning()
			return nil, ctx.Err()
		case ev, ok := <-stream:
			if !ok {
				closeNativeReasoning()
				return finishStream(final, text.String(), reasoning.String(), partials, order), nil
			}
		switch ev.Kind {
			case core.StreamText:
				if firstEvent != nil { firstEvent(); firstEvent = nil }
				text.WriteString(ev.Text)
				// Close BEFORE forwarding the text, so the channel ends the
				// reasoning group rather than streaming the answer into it.
				if ev.Text != "" {
					closeNativeReasoning()
				}
				hook.OnTextDelta(ctx, ev.Text)
			case core.StreamReasoning:
				if firstEvent != nil { firstEvent(); firstEvent = nil }
				reasoning.WriteString(ev.Text)
				hook.OnReasoningDelta(ctx, ev.Text)
				if ev.Text != "" {
					*streamedReasoning = true
					nativeReasoningOpen = true
				}
			case core.StreamToolCall:
				if firstEvent != nil { firstEvent(); firstEvent = nil }
				p, exists := partials[ev.Index]
				if !exists {
					p = &streamPartial{}
					partials[ev.Index] = p
					order = append(order, ev.Index)
				}
				if ev.ToolCallID != "" {
					p.id = ev.ToolCallID
				}
				if ev.ToolCallName != "" {
					p.name = ev.ToolCallName
				}
				p.args.WriteString(ev.ArgumentsDelta)
			case core.StreamDone:
				if firstEvent != nil { firstEvent(); firstEvent = nil }
				if ev.Response != nil {
					final = ev.Response
				}
			}
		}
	}
}

// finishStream builds the aggregated response when the provider did not supply
// one on the done event.
func finishStream(final *core.Response, text, reasoning string, partials map[int]*streamPartial, order []int) *core.Response {
	if final != nil {
		return final
	}
	resp := &core.Response{Content: text, FinishReason: core.FinishStop}
	if text != "" {
		resp.HasContent = true
	}
	if reasoning != "" {
		resp.ReasoningContent = reasoning
	}
	for _, idx := range order {
		p := partials[idx]
		// The reference counterpart is parse_tool_arguments (base.py:110-130),
		// which the OpenAI-compatible stream assembler calls on the accumulated
		// argument string: `stripped = arguments.strip()` and `if not stripped:
		// return {}` (base.py:122-124). That is a Python .strip(), so
		// strings.TrimSpace would treat U+001C..U+001F as argument text where
		// the reference sees only whitespace and yields a no-arg call.
		//
		// Residual (pre-existing, unchanged here): the reference then parses
		// `stripped` but returns the ORIGINAL unstripped string when the JSON
		// is malformed, whereas this port substitutes the stripped string as
		// the payload in every case.
		args := textutil.PyStrip(p.args.String())
		if args == "" {
			args = "{}"
		}
		resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{
			ID:        p.id,
			Name:      p.name,
			Arguments: json.RawMessage(args),
		})
	}
	if len(resp.ToolCalls) > 0 {
		resp.FinishReason = core.FinishToolCalls
	}
	return resp
}

// executeTools runs tool calls and returns their results in call order.
//
// Sequential by default, matching the reference. Parallel execution requires
// ConcurrentTools and is further restricted to tools that declare themselves
// concurrency-safe; a single unsafe or exclusive call forces the whole batch to
// run sequentially, because partial parallelism would change observable
// ordering in ways the reference never produces.
func (r *Runner) executeTools(
	ctx context.Context,
	spec RunSpec,
	calls []core.ToolCall,
	hook Hook,
) []core.ToolResult {
	started := time.Now()
	defer func() {
		if spec.Metrics != nil { spec.Metrics.ObserveTools(time.Since(started)) }
	}()
	if spec.Tools == nil {
		out := make([]core.ToolResult, len(calls))
		for i, c := range calls {
			out[i] = core.ToolResult{
				CallID:  c.ID,
				Content: fmt.Sprintf("Error: unknown tool %q (no tools registered).", c.Name),
				IsError: true,
			}
		}
		return out
	}

	// Partition into batches, mirroring _partition_tool_batches
	// (tools/execution.py:292-316): consecutive concurrency-safe calls form one
	// parallel batch, while every non-concurrency-safe call runs strictly
	// alone. Results are always written back in the model's call order.
	batches := partitionBatches(spec, calls)

	out := make([]core.ToolResult, len(calls))
	for _, batch := range batches {
		if len(batch) == 1 || !spec.ConcurrentTools {
			for _, i := range batch {
				out[i] = r.runOne(ctx, spec, calls[i], hook)
			}
			continue
		}

		var sem chan struct{}
		if spec.MaxParallelTools > 0 {
			sem = make(chan struct{}, spec.MaxParallelTools)
		}
		var wg sync.WaitGroup
		for _, i := range batch {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if sem != nil {
					select {
					case sem <- struct{}{}:
						defer func() { <-sem }()
					case <-ctx.Done():
						out[i] = core.ToolResult{
							CallID:  calls[i].ID,
							Content: fmt.Sprintf("Error: tool %q canceled: %v", calls[i].Name, ctx.Err()),
							IsError: true,
						}
						return
					}
				}
				out[i] = r.runOne(ctx, spec, calls[i], hook)
			}(i)
		}
		wg.Wait()
	}
	return out
}

// partitionBatches groups call indices into execution batches. Consecutive
// concurrency-safe calls are grouped; anything else is a singleton batch.
func partitionBatches(spec RunSpec, calls []core.ToolCall) [][]int {
	var batches [][]int
	var current []int

	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
		}
	}

	for i, c := range calls {
		t, ok := spec.Tools.Get(c.Name)
		safe := ok && tools.IsConcurrencySafe(t) && !tools.IsExclusive(t)
		if safe {
			current = append(current, i)
			continue
		}
		flush()
		batches = append(batches, []int{i})
	}
	flush()
	return batches
}

// runOne executes a single tool call with hook notifications and truncation.
func (r *Runner) runOne(
	ctx context.Context,
	spec RunSpec,
	call core.ToolCall,
	hook Hook,
) core.ToolResult {
	hook.OnToolStart(ctx, call)
	res := spec.Tools.Execute(tools.WithRequestRoute(ctx, tools.RequestRoute{SessionKey: spec.SessionKey, Channel: spec.Channel, ChatID: spec.ChatID, Metadata: spec.Metadata}), call)
	// NOTE: no truncation here. The reference bounds tool results only inside
	// normalize_tool_result (context_governance.py:709), which offloads to the
	// workspace when one exists. Truncating here as well would destroy the
	// content before it could be persisted and would use a marker the
	// reference never emits.
	hook.OnToolEnd(ctx, call, res)
	return res
}

// assistantMessage renders a model response as a transcript message.
//
// Two things about it are load-bearing and were both wrong before:
//
//   - the content is the CLEANED text (`clean`), not resp.Content. The
//     reference builds every persisted assistant turn with
//     build_assistant_message(clean, ...) — runner.py:646-653 for a
//     length-recovery segment and :756-766 for the terminal one — and `clean`
//     is `hook.finalize_content(context, response.content)`, i.e.
//     `strip_think(content) or None` (progress_hook.py:46-49, :180-181).
//     Persisting the raw content instead put boundary whitespace and inline
//     thinking tags into the transcript, where they are replayed to the
//     provider on every later request. (The one site that passes the raw text
//     is the tool branch, which uses `response.content or ""` at
//     runner.py:488 — but that value was already replaced by
//     extract_reasoning's cleaned text at runner.py:473, so it is `clean`
//     again.)
//   - toolCalls is explicit. The reference persists tool calls only where it
//     means to: the issued calls in the tool branch (runner.py:489) and none at
//     all in the length-recovery (:646) and terminal (:756) branches.
func assistantMessage(resp *core.Response, clean string, toolCalls []core.ToolCall) core.Message {
	return core.Message{
		Role:             core.RoleAssistant,
		Content:          core.TextContent(clean),
		ToolCalls:        toolCalls,
		ReasoningContent: resp.ReasoningContent,
		ThinkingBlocks:   resp.ThinkingBlocks,
	}
}

// toolMessage renders a tool result as a transcript message.
//
// The reference emits exactly {"role":"tool","tool_call_id","name","content"}
// (runner.py:533-543). The `name` field is required by some OpenAI-compatible
// backends, so it is not optional.
func toolMessage(spec RunSpec, tr core.ToolResult, toolName string) core.Message {
	// tool_call_id is a MODELLED field on core.Message, so it must be assigned
	// to the struct field. Writing it through SetExtra only populates the
	// unknown-key store, which MarshalJSON deliberately ignores for modelled
	// keys — the id would silently serialize as "".
	//
	// Content is normalized here because this is exactly where the reference
	// does it (runner.py:537): an empty result becomes a marker, and an
	// oversized one is offloaded to the workspace so the model receives a
	// bounded reference it can follow rather than a silently cut payload.
	content := NormalizeToolResult(
		spec.Workspace, spec.SessionKey, tr.CallID, toolName,
		core.TextContent(tr.Content), spec.MaxToolResultChars,
	)
	return core.Message{
		Role:       core.RoleTool,
		Content:    content,
		ToolCallID: tr.CallID,
		Name:       toolName,
	}
}

// validCalls filters out degenerate tool calls.
func validCalls(calls []core.ToolCall) []core.ToolCall {
	out := make([]core.ToolCall, 0, len(calls))
	for _, c := range calls {
		if c.HasValidName() {
			out = append(out, c)
		}
	}
	return out
}

// MaxLengthRecoveries is _MAX_LENGTH_RECOVERIES (runner.py:72): how many times
// a truncated answer may be continued before it is accepted as it stands.
const MaxLengthRecoveries = 3

// LengthRecoveryTailChars is _LENGTH_RECOVERY_TAIL_CHARS (runtime.py:17): how
// much of the already-delivered text is quoted back to the model so it can
// resume from the right place.
const LengthRecoveryTailChars = 64

// LengthRecoveryPrompt is LENGTH_RECOVERY_PROMPT (runtime.py:35). The wording
// matters: without the explicit ban on restarting and recapping, models tend to
// re-emit the answer from the top and the user sees it twice.
const LengthRecoveryPrompt = "The previous assistant response was cut off. " +
	"Continue the same response from its exact endpoint. Output only new " +
	"continuation text in the same language and style. Do not acknowledge this " +
	"instruction, restart the response, repeat its title or any existing text, " +
	"recap, or apologize."

// IsBlankText ports is_blank_text (utils/runtime.py:63): "content is None or
// not content.strip()".
//
// The Go parameter is a plain string, so None is spelled "" — the same
// collapse the reference's own `or None` in AgentProgressHook._strip_think
// performs. The strip is Python's (textutil.PyStrip), not Go's: for a lone
// "\x1c" this reports blank, while strings.TrimSpace would not. (NBSP U+00A0 is
// whitespace for both, so it is not one of the divergences.)
//
// Callers that already hold a `clean` (which StripThink has stripped) get the
// same answer from `clean == ""`; this function exists so the reference's
// predicate has one named, differentially tested home.
func IsBlankText(content string) bool {
	return textutil.PyStrip(content) == ""
}

// EmptyRetryEligible is the reference's empty-content retry predicate,
// runner.py:588-593:
//
//	if (response.finish_reason not in {"error", "length", "refusal", "content_filter"}
//	        and is_blank_text(clean)):
//
// It is deliberately NOT the whole condition: the reference reaches this point
// only for a response the tool-execution branch above it did not consume
// (runner.py:~480-578 ends in `continue`), which the runner expresses as
// !resp.HasToolCalls(). Keeping the two halves separate means the finish_reason
// exclusion is stated once, in the reference's own terms, and can be compared
// against the Python truth table exhaustively.
func EmptyRetryEligible(finishReason core.FinishReason, clean string) bool {
	switch finishReason {
	case core.FinishError, core.FinishLength, core.FinishRefusal, core.FinishContentFilter:
		// Not a "the model produced nothing" case: error and length have their
		// own recovery paths, and refusal/content_filter are deliberate
		// provider outcomes that a retry would only repeat.
		return false
	}
	return IsBlankText(clean)
}

// RestoreOuterWhitespace ports _restore_outer_whitespace (runner.py:77): put
// back the boundary whitespace that cleaning stripped from one recovered
// segment, so concatenating the segments does not glue words together.
//
// The cutset is Python's, not ASCII's. Using the six ASCII characters here was
// a real divergence: for "\u00a0hi" the reference returns "\u00a0X" and the
// ASCII version returned "X", silently dropping the boundary character. See
// textutil.PyStrip for the derivation of the set.
//
// Byte arithmetic is used for the two sizes, which is equivalent to the
// reference's character arithmetic because the removed prefix and suffix are
// exactly the leading and trailing runs of whitespace either way.
//
// Note that an all-whitespace original DOUBLES the whitespace around content.
// That is what the reference does (leading and trailing both span the whole
// string), so it is reproduced rather than corrected.
func RestoreOuterWhitespace(content, original string) string {
	if original == "" {
		return content
	}
	leadingSize := len(original) - len(textutil.PyLStrip(original))
	trailingSize := len(original) - len(textutil.PyRStrip(original))
	leading := original[:leadingSize]
	trailing := ""
	if trailingSize > 0 {
		trailing = original[len(original)-trailingSize:]
	}
	return leading + content + trailing
}

// BuildLengthRecoveryMessage ports build_length_recovery_message
// (runtime.py:78).
func BuildLengthRecoveryMessage(content string) core.Message {
	tail := content
	if len(tail) > LengthRecoveryTailChars {
		// Sliced by runes, not bytes: a byte cut through a multi-byte
		// character would send invalid UTF-8 to the provider.
		runes := []rune(tail)
		if len(runes) > LengthRecoveryTailChars {
			tail = string(runes[len(runes)-LengthRecoveryTailChars:])
		}
	}
	prompt := LengthRecoveryPrompt + "\n\n" +
		"The following tail was already delivered to the user. Treat it as immutable " +
		"context and do not output it again:\n" +
		"<already_delivered_tail>\n" + tail + "\n</already_delivered_tail>\n" +
		"Begin with the text that belongs immediately after this tail."
	return *core.NewMessage(core.RoleUser, prompt)
}

// BudgetExhaustedFinalizationPrompt is BUDGET_EXHAUSTED_FINALIZATION_PROMPT
// (utils/runtime.py:28). The wording is reproduced verbatim: it is the
// instruction that keeps the model from claiming a task is complete when the
// evidence above does not support it.
const BudgetExhaustedFinalizationPrompt = "The tool-call budget for this turn is exhausted. " +
	"Based only on the conversation and tool results above, provide a concise final " +
	"response to the user. Do not call or request tools. Do not claim the task is " +
	"complete unless the evidence above clearly shows it is complete. State what was " +
	"done, what remains, and the best next step if anything is incomplete."

// shouldFinalizeOnMaxIterations mirrors AgentRunSpec.finalize_on_max_iterations,
// whose default is true (runner.py:112).
func shouldFinalizeOnMaxIterations(spec RunSpec) bool {
	if spec.FinalizeOnMaxIterations != nil {
		return *spec.FinalizeOnMaxIterations
	}
	return true
}

// finalizeAfterMaxIterations asks for a final answer with no tools available.
//
// Mirrors _try_finalize_after_max_iterations (runner.py:1163). Returns ok=false
// when the attempt is unusable, in which case the caller falls back to the
// static notice. Three conditions must all hold for the answer to be accepted,
// exactly as in the reference:
//
//   - the request must not have failed;
//   - the model must not have asked for tools it cannot have (a response with
//     tool calls here means the prompt was ignored, and executing them would
//     contradict "the budget is exhausted");
//   - the content must not be blank.
//
// A failure of the attempt itself is not an error: the caller has a fallback,
// and turning a salvage attempt into a hard failure would be worse than the
// static notice.
func (r *Runner) finalizeAfterMaxIterations(
	ctx context.Context,
	spec RunSpec,
	messages []core.Message,
	hook Hook,
) (string, bool) {
	if !shouldFinalizeOnMaxIterations(spec) {
		return "", false
	}

	// Tools must be unavailable, so a second request spec is built with none.
	// This also keeps the governance estimator honest: the schemas are not
	// sent, so they must not be counted.
	noTools := spec
	noTools.Tools = nil

	retryMessages := make([]core.Message, 0, len(messages)+1)
	retryMessages = append(retryMessages, messages...)
	retryMessages = append(retryMessages, *core.NewMessage(core.RoleUser, BudgetExhaustedFinalizationPrompt))

	// The finalize request has no reasoning gate to honour: the reference's
	// _try_finalize_after_max_iterations never calls extract_reasoning and
	// never emits reasoning (runner.py:1207-1215 — it goes straight to
	// hook.finalize_content), and it issues the request WITHOUT the streaming
	// callbacks. The flag is therefore local and discarded. The extraction is
	// deliberately NOT run here either, so `clean` below stays the single
	// strip_think of the raw content that the reference performs.
	var streamedReasoning bool
	resp, err := r.requestModelOnce(ctx, noTools, retryMessages, hook, &streamedReasoning)
	if err != nil || resp == nil {
		return "", false
	}
	if resp.FinishReason == core.FinishError || len(resp.ToolCalls) > 0 {
		return "", false
	}
	// runner.py:1214-1217 computes clean through the hook chain and returns
	// `clean`, not the raw content, so the salvaged answer is delivered with
	// its thinking blocks removed and its boundary whitespace trimmed.
	clean := textutil.StripThink(resp.Content)
	if IsBlankText(clean) {
		return "", false
	}
	return clean, true
}

// maxIterationsMessage renders the budget-exhausted notice.
// Mirrors nanobot/templates/agent/max_iterations_message.md.
func maxIterationsMessage(spec RunSpec) string {
	if spec.MaxIterationsMessage != "" {
		return strings.ReplaceAll(spec.MaxIterationsMessage, "{max_iterations}",
			fmt.Sprintf("%d", spec.MaxIterations))
	}
	return fmt.Sprintf(
		"I reached the maximum number of tool call iterations (%d) without completing the task. "+
			"You can try breaking the task into smaller steps.", spec.MaxIterations)
}

// isArrearage reports whether a response indicates quota exhaustion.
// Mirrors LLMProvider.is_arrearage_response (base.py:1031).
func isArrearage(r *core.Response) bool {
	if r == nil {
		return false
	}
	if r.ErrorType == "insufficient_quota" {
		return true
	}
	lower := strings.ToLower(r.Content + " " + r.ErrorCode + " " + r.ErrorType)
	for _, needle := range []string{"insufficient_quota", "out of quota", "arrears", "billing"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// errorResponseFromException converts a transport failure into an error
// response, mirroring LLMProvider._error_response_from_exception (base.py:950).
//
// The content prefix is part of the observable contract: the runner surfaces
// this text to the user when it is non-blank, so it must match the reference
// wording rather than an ad-hoc Go message.
func errorResponseFromException(err error) *core.Response {
	// base.py:994 — `detail = str(exc).strip() or type(exc).__name__`.
	//
	// The strip is Python's, not Go's: str.strip() also removes
	// U+001C..U+001F, so an exception whose message is only those characters
	// falls through to the type-name branch in the reference but would have
	// been kept as "content" by strings.TrimSpace.
	//
	// The fallback is the exception CLASS NAME, not a fixed "unknown error"
	// literal. Go has no exception classes, so the closest analogue of
	// `type(exc).__name__` is the dynamic type's unqualified name
	// (goErrorTypeName below). Limits of that choice are documented there.
	detail := textutil.PyStrip(err.Error())
	if detail == "" {
		detail = goErrorTypeName(err)
	}

	resp := &core.Response{
		Content:      "Error calling LLM: " + detail,
		FinishReason: core.FinishError,
	}

	// Classify the error kind so the retry policy can act on it. Go has no
	// exception MRO, so this uses the structured error types first and falls
	// back to message inspection, which is the same order the Python version
	// effectively achieves via error metadata then text markers.
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		code := httpErr.StatusCode
		resp.ErrorStatusCode = &code
		switch {
		case code == 429:
			resp.ErrorKind = "rate_limit"
		case code >= 500:
			resp.ErrorKind = "server_error"
		case code == 401 || code == 403:
			resp.ErrorKind = "authentication"
		}
		retryable := httpErr.Retryable()
		resp.ErrorShouldRetry = &retryable
		if httpErr.RetryAfter > 0 {
			ra := httpErr.RetryAfter
			resp.ErrorRetryAfterS = &ra
			resp.RetryAfter = &ra
		}
		return resp
	}

	if errors.Is(err, context.DeadlineExceeded) {
		resp.ErrorKind = "timeout"
		retry := true
		resp.ErrorShouldRetry = &retry
		return resp
	}

	transient := provider.IsTransientErrorText(detail)
	if transient {
		resp.ErrorKind = "connection"
		resp.ErrorShouldRetry = &transient
	}
	return resp
}

// goErrorTypeName is the Go analogue of Python's `type(exc).__name__` in
// base.py:994 (`detail = str(exc).strip() or type(exc).__name__`).
//
// It returns the dynamic type's UNQUALIFIED name, unwrapping pointer
// indirection, because Python's __name__ is the bare class name with no module
// path — `type(ValueError("")).__name__` is "ValueError", not
// "builtins.ValueError". Unwrapping pointers is what makes `*url.Error` and
// `url.Error` agree, mirroring the fact that Python has no value/reference
// distinction here: an exception instance and its class share one name.
//
// LIMITS — this is an approximation, and the divergence is inherent rather
// than a defect in this function:
//
//   - Go has no exception class hierarchy. Python's __mro__ (used a few lines
//     above for error_kind) has no counterpart, so a single name is all that
//     can be recovered.
//   - The name is the GO type's, not the reference's. Where Python would say
//     "ValueError" or "FileNotFoundError", Go says "errorString" (errors.New),
//     "wrapError" (fmt.Errorf with %w) or "PathError". There is no mapping that
//     would be correct in general, and inventing one would fabricate a
//     correspondence the languages do not have.
//   - The fallback is reachable only for an error whose message is empty or
//     entirely Python-whitespace (e.g. errors.New("")), which is rare but not
//     impossible. For every ordinary error the message branch wins and the
//     reference's wording is reproduced byte-for-byte.
//
// It returns "unknown error" only if the type has no name at all (an anonymous
// type), which keeps the response content non-empty in every case.
func goErrorTypeName(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return "unknown error"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if name := t.Name(); name != "" {
		return name
	}
	if s := t.String(); s != "" {
		return s
	}
	return "unknown error"
}

// AppendFinalMessage ports AgentRunner._append_final_message (runner.py:1356):
// the one place the reference writes a terminal assistant turn to the
// transcript. It is called by the blank-response branch (runner.py:741) and by
// the max-iterations branch (runner.py:823).
//
// The rule is deliberately NOT "append", and both of the other outcomes are
// reachable:
//
//	if not content: return                      # "" and None are no-ops
//	if last is assistant without tool_calls:
//	    if last.content == content: return      # already recorded
//	    replace the last message                # do not stack two assistant turns
//	append
//
// The replace branch is what keeps a transcript from ending in two consecutive
// assistant turns, which some providers reject outright.
//
// Python truthiness decides the first guard, so a whitespace-only or
// U+001C-only content is still written: `not "\x1c"` is False in Python and
// "\x1c" != "" in Go.
func AppendFinalMessage(messages []core.Message, content string) []core.Message {
	if content == "" {
		return messages
	}
	if n := len(messages); n > 0 {
		last := messages[n-1]
		if last.Role == core.RoleAssistant && len(last.ToolCalls) == 0 {
			// `messages[-1].get("content") == content`: a block-list content is
			// never equal to a string, so only a text turn can match.
			if last.Content.IsText() && last.Content.Text == content {
				return messages
			}
			messages[n-1] = *core.NewMessage(core.RoleAssistant, content)
			return messages
		}
	}
	return append(messages, *core.NewMessage(core.RoleAssistant, content))
}

// AppendModelErrorPlaceholder records a well-formed assistant turn after a
// failed model call, mirroring _append_model_error_placeholder (runner.py:1371).
// Without it the transcript would end on a user turn with no reply, which
// breaks strict role alternation on the next request.
//
// Unlike AppendFinalMessage this never replaces: an assistant turn without tool
// calls already answers the user, so the placeholder is only written when the
// transcript does not already end in one.
func AppendModelErrorPlaceholder(messages []core.Message) []core.Message {
	if n := len(messages); n > 0 {
		last := messages[n-1]
		if last.Role == core.RoleAssistant && len(last.ToolCalls) == 0 {
			return messages
		}
	}
	return append(messages, *core.NewMessage(core.RoleAssistant, PersistedModelErrorPlaceholder))
}

// SleepWithContext waits for d or until ctx is done, whichever comes first.
// Exported so callers implementing their own retry can reuse it.
func SleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrNilProvider is returned when a run is started without a provider.
var ErrNilProvider = errors.New("agent: provider is nil")
