package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/bpe"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// Context governance: the model-facing copy of a transcript.
//
// Ports nanobot/agent/context_governance.py. The reference keeps a raw
// transcript on disk and builds a separate, *repaired* copy for each provider
// request. The repair matters because upstream APIs reject whole requests for
// structural defects that are easy to accumulate in a long session:
//
//   - a `tool` message whose tool_call_id was never declared by a preceding
//     assistant message, or that duplicates an already-fulfilled id;
//   - an assistant tool_call with no matching tool result;
//   - an assistant tool_call with a missing or empty function name;
//   - a compaction placeholder replayed as if it were a real turn.
//
// Any one of these permanently wedges a session: every subsequent request
// fails, and the failure is not caused by the new turn. The persisted
// transcript is never modified — only the copy sent to the model.
//
// This is the same failure class as the empty `tool_call_id` bug found by the
// e2e harness; that was one instance, this is the general guard.

// Governance constants, verbatim from context_governance.py.
const (
	// SnipSafetyBuffer is SNIP_SAFETY_BUFFER (context_governance.py:67).
	SnipSafetyBuffer = 1024

	// BackfillContent is BACKFILL_CONTENT (context_governance.py:70). The
	// em dash is part of the string and must be preserved: the model reads it.
	BackfillContent = "[Tool result unavailable — call was interrupted or lost]"

	// defaultMaxOutputTokens is the fallback used when neither the config nor
	// the provider declares a max_tokens (context_governance.py:699-704).
	defaultMaxOutputTokens = 4096
)

// placeholderTexts mirrors PLACEHOLDER_TEXTS (context_governance.py:71).
var placeholderTexts = map[string]bool{
	"[Previous assistant message omitted.]": true,
}

// ContextWindowExceededError mirrors the reference's exception of the same name
// (context_governance.py:76). It is raised only after a request has already
// been fitted to the budget and still does not fit, which means the single
// newest turn alone exceeds the window.
type ContextWindowExceededError struct {
	SessionKey      string
	EstimatedTokens int
	InputBudget     int
	Source          string
}

func (e *ContextWindowExceededError) Error() string {
	key := e.SessionKey
	if key == "" {
		key = "default"
	}
	return fmt.Sprintf(
		"model input still exceeds the local context budget after request fitting "+
			"for %s: %d/%d via %s",
		key, e.EstimatedTokens, e.InputBudget, e.Source)
}

// ---------------------------------------------------------------------------
// Token estimation
// ---------------------------------------------------------------------------

// EstimateMessageTokens estimates the token cost of one message.
//
// Mirrors estimate_message_tokens (helpers.py:783): the message's parts are
// joined with newlines, encoded with cl100k_base, and reported as
// max(4, tokens+4) — the +4 is per-message framing. When the encoder raises —
// which it does for any text containing one of the five special token strings,
// because tiktoken's default is disallowed_special="all" — the same formula is
// applied to the UTF-8 byte count instead.
//
// The parts and their ORDER come from messageTokensParts, which is not the same
// list _estimate_prompt_tokens_with_source builds. See the comment there.
func EstimateMessageTokens(m core.Message) int {
	payload := joinWithNewline(messageTokensParts(m))
	if payload == "" {
		return 4
	}
	if n, err := bpe.TokenCount(payload); err == nil {
		return max(4, n+4)
	}
	return max(4, len(payload)+4)
}

// EstimatePromptTokens estimates the whole request, including tool definitions.
//
// Mirrors _estimate_prompt_tokens_with_source (helpers.py:719). The second
// return value is the counter that produced the number:
//
//	"tiktoken"   the real cl100k_base encoder was used
//	"heuristic"  the reference's `except Exception` byte fallback was used
//
// That string is not decoration — EnsureRequestFits reports it inside
// ContextWindowExceededError — and "heuristic" means the number is a
// conservative over-estimate rather than a measurement, because one UTF-8 byte
// is charged per token.
//
// The payload is every message's parts joined with "\n", then the serialized
// tool definitions. The join spans ALL messages: two messages "a" and "b"
// contribute "a\nb", not "ab".
func EstimatePromptTokens(messages []core.Message, tools []provider.ToolSchema) (int, string) {
	return estimatePromptTokensWithSource(messages, tools)
}

// joinWithNewline mirrors "\n".join(parts): no trailing separator.
func joinWithNewline(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts, "\n")
}

// marshalSpaced serializes v the way Python's json.dumps(..., ensure_ascii=False)
// does: no HTML escaping, and with the DEFAULT ", " and ": " separators rather
// than Go's compact ",/:" form. The reference's token estimators
// (estimate_message_tokens, helpers.py:783; _estimate_prompt_tokens_with_source,
// helpers.py:719) feed json.dumps output straight into the tokenizer, so the
// byte count must match exactly — a compact separator would under-count by the
// spaces Python inserts. Verified: for {"type":"image_url","image_url":{"url":"x"}}
// the reference reports 21 tokens and tiktoken counts 17 on the spaced string
// vs 13 on the compact one.
func marshalSpaced(v any) ([]byte, error) {
	compact, err := marshalCompactNoEscape(v)
	if err != nil {
		return nil, err
	}
	return spacedJSON(compact), nil
}

// marshalCompactNoEscape is the no-HTML-escape compact base that spacedJSON
// post-processes. It mirrors the bytes Python's json.dumps(ensure_ascii=False)
// would produce for the same value, minus the separators.
func marshalCompactNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// spacedJSON inserts Python's default JSON separators (", " and ": ") into a
// compact JSON document, touching only structural separators. It tracks string
// state so that ':' and ',' inside string values (e.g. URLs) are left alone.
// Outside strings, ':' only ever appears as a key separator and ',' only as a
// value separator, so this is exact for valid JSON.
//
// Known limitation: Python's json.dumps escapes U+2028/U+2029 as \u2028/\u2029
// while Go's encoding/json does not. That divergence is irrelevant to the
// token-estimation corpus and is documented here rather than worked around.
func spacedJSON(b []byte) []byte {
	out := make([]byte, 0, len(b)+bytes.Count(b, []byte{','})+bytes.Count(b, []byte{':'}))
	inString := false
	escaped := false
	for _, c := range b {
		switch {
		case escaped:
			out = append(out, c)
			escaped = false
		case c == '\\':
			out = append(out, c)
			escaped = true
		case c == '"':
			out = append(out, c)
			inString = !inString
		case !inString && c == ':':
			out = append(out, ':', ' ')
		case !inString && c == ',':
			out = append(out, ',', ' ')
		default:
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// History repair
// ---------------------------------------------------------------------------

// toolCallNameIsValid mirrors _tool_call_name_is_valid (context_governance.py:97).
// A degenerate call with an empty name cannot be executed and is rejected by
// upstream APIs if replayed.
func toolCallNameIsValid(tc core.ToolCall) bool {
	return tc.Name != ""
}

// StripPlaceholderAssistantMessages removes compaction placeholders.
//
// Mirrors strip_placeholder_assistant_messages (context_governance.py:762).
// A placeholder carries no usable context and can make the model retry tool
// calls that previously failed, looping on malformed responses.
//
// Returns the input slice unchanged when nothing is removed.
func StripPlaceholderAssistantMessages(messages []core.Message) []core.Message {
	var out []core.Message
	changed := false
	for i, m := range messages {
		if m.Role != core.RoleAssistant {
			if changed {
				out = append(out, m)
			}
			continue
		}
		text := ""
		if m.Content.IsText() {
			text = m.Content.Text
		}
		// A placeholder that still carries tool calls is kept: the calls are
		// real and their results must stay correlated.
		//
		// The strip is Python's (context_governance.py:783, `text.strip() in
		// PLACEHOLDER_TEXTS`), not Go's: a placeholder wrapped in
		// U+001C..U+001F is recognised by the reference and would not be by
		// strings.TrimSpace.
		if placeholderTexts[textutil.PyStrip(text)] && len(m.ToolCalls) == 0 {
			if !changed {
				// Copy everything BEFORE this index. Using len(out) here would
				// be wrong: out is still empty at the first removal, so every
				// preceding message would be lost.
				out = append([]core.Message(nil), messages[:i]...)
				changed = true
			}
			continue
		}
		if changed {
			out = append(out, m)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// StripMalformedToolCalls drops assistant tool_calls whose name is empty.
//
// Mirrors strip_malformed_tool_calls (context_governance.py:800). Such a call
// makes upstream APIs reject the entire request, permanently wedging the
// session. Removing it here lets DropOrphanToolResults drop the now-dangling
// result, so a polluted session heals on its next turn.
func StripMalformedToolCalls(messages []core.Message) []core.Message {
	var out []core.Message
	changed := false
	for i, m := range messages {
		if m.Role != core.RoleAssistant || len(m.ToolCalls) == 0 {
			if changed {
				out = append(out, m)
			}
			continue
		}
		kept := make([]core.ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if toolCallNameIsValid(tc) {
				kept = append(kept, tc)
			}
		}
		if len(kept) == len(m.ToolCalls) {
			if changed {
				out = append(out, m)
			}
			continue
		}
		if !changed {
			// Copy everything before this index; see the note in
			// StripPlaceholderAssistantMessages.
			out = append([]core.Message(nil), messages[:i]...)
			changed = true
		}
		repaired := m
		repaired.ToolCalls = kept
		// An assistant turn with neither content nor a valid call is itself
		// invalid upstream, so drop it entirely (context_governance.py:845).
		if len(kept) == 0 && !messageHasContent(m) {
			continue
		}
		out = append(out, repaired)
	}
	if !changed {
		return messages
	}
	return out
}

// DropOrphanToolResults removes tool messages that no assistant turn declared,
// or that duplicate an id already fulfilled.
//
// Mirrors drop_orphan_tool_results (context_governance.py:855). Note that
// declaration is positional: a tool result appearing before its declaring
// assistant message is dropped, exactly as in the reference.
func DropOrphanToolResults(messages []core.Message) []core.Message {
	declared := map[string]bool{}
	fulfilled := map[string]bool{}
	var out []core.Message
	changed := false

	for i, m := range messages {
		if m.Role == core.RoleAssistant {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					declared[tc.ID] = true
				}
			}
		}
		if m.Role == core.RoleTool {
			id := m.ToolCallID
			if id == "" || !declared[id] || fulfilled[id] {
				if !changed {
					out = append([]core.Message(nil), messages[:i]...)
					changed = true
				}
				continue
			}
			fulfilled[id] = true
		}
		if changed {
			out = append(out, m)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// BackfillMissingToolResults inserts synthetic error results for assistant
// tool_calls that never received one.
//
// Mirrors backfill_missing_tool_results (context_governance.py:886). An
// assistant turn declaring a call with no matching result is rejected by
// upstream APIs; the synthetic result keeps the transcript structurally valid
// without pretending the tool succeeded.
func BackfillMissingToolResults(messages []core.Message) []core.Message {
	type decl struct {
		index int
		id    string
		name  string
	}
	var declared []decl
	fulfilled := map[string]bool{}

	for i, m := range messages {
		switch m.Role {
		case core.RoleAssistant:
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					declared = append(declared, decl{index: i, id: tc.ID, name: tc.Name})
				}
			}
		case core.RoleTool:
			if m.ToolCallID != "" {
				fulfilled[m.ToolCallID] = true
			}
		}
	}

	var missing []decl
	for _, d := range declared {
		if !fulfilled[d.id] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return messages
	}

	out := append([]core.Message(nil), messages...)
	offset := 0
	for _, d := range missing {
		insertAt := d.index + 1 + offset
		// Skip past any tool results already sitting after this assistant turn.
		for insertAt < len(out) && out[insertAt].Role == core.RoleTool {
			insertAt++
		}
		filler := core.NewMessage(core.RoleTool, BackfillContent)
		filler.ToolCallID = d.id
		filler.Name = d.name
		out = append(out, core.Message{})
		copy(out[insertAt+1:], out[insertAt:])
		out[insertAt] = *filler
		offset++
	}
	return out
}

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

// InputBudget returns the token budget available for input.
//
// Mirrors input_budget (context_governance.py:693): the output reservation and
// a safety buffer are subtracted from the context window. A non-positive
// result means "no budget information", which disables governance rather than
// rejecting every request.
//
// maxTokens is a POINTER because the reference distinguishes an absent
// max_tokens from an explicit zero: the guard is
// `isinstance(config.max_tokens, int)`, and 0 IS an int, so an explicit 0
// reserves nothing. Passing a plain int and treating 0 as "unset" would
// silently shrink the budget by 4096 tokens and compact earlier than the
// reference. nil means unset and falls back to defaultMaxOutputTokens.
func InputBudget(contextWindowTokens int, maxTokens *int) int {
	if contextWindowTokens == 0 {
		return 0
	}
	maxOutput := defaultMaxOutputTokens
	if maxTokens != nil {
		maxOutput = *maxTokens
	}
	budget := contextWindowTokens - maxOutput - SnipSafetyBuffer
	if budget <= 0 {
		return 0
	}
	return budget
}

// FindLegalMessageStart returns the first index whose tool results have
// matching assistant calls.
//
// Mirrors find_legal_message_start (helpers.py:478). Used after trimming so the
// retained tail does not begin with a tool result whose declaring call was cut.
func FindLegalMessageStart(messages []core.Message) int {
	declared := map[string]bool{}
	start := 0
	for i, m := range messages {
		switch m.Role {
		case core.RoleAssistant:
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					declared[tc.ID] = true
				}
			}
		case core.RoleTool:
			if m.ToolCallID != "" && !declared[m.ToolCallID] {
				start = i + 1
				declared = map[string]bool{}
			}
		}
	}
	return start
}

// userTail returns the slice starting at the first user message, or from the
// last user message when last is true. Mirrors _user_tail
// (context_governance.py:1013). Returns nil when there is no user message.
func userTail(messages []core.Message, last bool) []core.Message {
	if last {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == core.RoleUser {
				return messages[i:]
			}
		}
		return nil
	}
	for i := range messages {
		if messages[i].Role == core.RoleUser {
			return messages[i:]
		}
	}
	return nil
}

// SnipHistory trims the oldest non-system messages until the request fits.
//
// Mirrors snip_history (context_governance.py:951). System messages are always
// retained; the newest messages are kept preferentially, and the retained tail
// is then adjusted to a legal boundary that starts with a user turn.
//
// force skips the initial "does it even need trimming" check, which is what
// fit_to_budget does before validating.
func SnipHistory(messages []core.Message, tools []provider.ToolSchema, contextWindowTokens int, maxTokens *int, force bool) []core.Message {
	if len(messages) == 0 || contextWindowTokens == 0 {
		return messages
	}
	budget := InputBudget(contextWindowTokens, maxTokens)
	if budget <= 0 {
		return messages
	}
	if !force {
		estimate, _ := EstimatePromptTokens(messages, tools)
		if estimate <= budget {
			return messages
		}
	}

	var system, nonSystem []core.Message
	for _, m := range messages {
		if m.Role == core.RoleSystem {
			system = append(system, m)
		} else {
			nonSystem = append(nonSystem, m)
		}
	}
	if len(nonSystem) == 0 {
		return messages
	}

	systemTokens := 0
	for _, m := range system {
		systemTokens += EstimateMessageTokens(m)
	}
	fixedTokens, _ := EstimatePromptTokens(system, tools)
	if fixedTokens > systemTokens {
		systemTokens = fixedTokens
	}
	remaining := budget - systemTokens
	if remaining < 0 {
		remaining = 0
	}

	var kept []core.Message
	keptTokens := 0
	for i := len(nonSystem) - 1; i >= 0; i-- {
		t := EstimateMessageTokens(nonSystem[i])
		if len(kept) > 0 && keptTokens+t > remaining {
			break
		}
		kept = append(kept, nonSystem[i])
		keptTokens += t
	}
	// Reverse into chronological order.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	tail := legalHistoryTail(kept, nonSystem)
	out := make([]core.Message, 0, len(system)+len(tail))
	out = append(out, system...)
	out = append(out, tail...)
	return out
}

// legalHistoryTail mirrors _legal_history_tail (context_governance.py:1001).
func legalHistoryTail(kept, nonSystem []core.Message) []core.Message {
	fallback := kept
	if len(fallback) == 0 {
		if len(nonSystem) > 0 {
			fallback = nonSystem[len(nonSystem)-1:]
		} else {
			fallback = nil
		}
	}

	result := userTail(kept, false)
	if len(result) == 0 {
		result = userTail(nonSystem, true)
	}
	if len(result) == 0 {
		result = fallback
	}

	start := FindLegalMessageStart(result)
	if start > 0 && start <= len(result) {
		return result[start:]
	}
	return result
}

// RequestPressure reports the measured size and whether the request is at or
// over budget. Mirrors request_pressure (context_governance.py:384): it returns
// false when governance is disabled or the request fits comfortably.
//
// The reference compares with `<` for the fitting decision, so a request
// exactly AT the budget counts as pressured.
func RequestPressure(messages []core.Message, tools []provider.ToolSchema, contextWindowTokens int, maxTokens *int) (measured int, pressured bool) {
	if contextWindowTokens == 0 {
		return 0, false
	}
	budget := InputBudget(contextWindowTokens, maxTokens)
	measured, _ = EstimatePromptTokens(messages, tools)
	if budget > 0 && measured < budget {
		return measured, false
	}
	return measured, true
}

// EnsureRequestFits validates an already-fitted request without dropping
// anything. Mirrors ensure_request_fits (context_governance.py:358).
func EnsureRequestFits(messages []core.Message, tools []provider.ToolSchema, contextWindowTokens int, maxTokens *int, sessionKey string) error {
	if contextWindowTokens == 0 {
		return nil
	}
	budget := InputBudget(contextWindowTokens, maxTokens)
	estimated, source := EstimatePromptTokens(messages, tools)
	if budget > 0 && estimated <= budget {
		return nil
	}
	return &ContextWindowExceededError{
		SessionKey:      sessionKey,
		EstimatedTokens: estimated,
		InputBudget:     budget,
		Source:          source,
	}
}

// FitToBudget trims and then re-validates, mirroring fit_to_budget
// (context_governance.py:336).
func FitToBudget(messages []core.Message, tools []provider.ToolSchema, contextWindowTokens int, maxTokens *int, sessionKey string) ([]core.Message, error) {
	updated := SnipHistory(messages, tools, contextWindowTokens, maxTokens, true)
	updated = DropOrphanToolResults(updated)
	updated = BackfillMissingToolResults(updated)
	if err := EnsureRequestFits(updated, tools, contextWindowTokens, maxTokens, sessionKey); err != nil {
		return nil, err
	}
	return updated, nil
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

// PrepareForModel builds the normalized model-facing copy of a transcript.
//
// Mirrors prepare_for_model (context_governance.py:325). The order matters:
// malformed calls are stripped before orphans are dropped, so the result of a
// removed call is itself removed rather than left dangling.
//
// NOTE: the reference additionally merges adjacent user messages here
// (_merge_adjacent_user_messages_for_model). That merge depends on the
// runtime-context marker machinery, which this port does not implement, so it
// is deliberately omitted. Consequence: consecutive user turns are sent as
// separate messages instead of being joined. Documented as a divergence.
func PrepareForModel(messages []core.Message) []core.Message {
	updated := StripPlaceholderAssistantMessages(messages)
	updated = StripMalformedToolCalls(updated)
	updated = DropOrphanToolResults(updated)
	updated = BackfillMissingToolResults(updated)
	return updated
}

// messageHasContent reports whether a message carries any non-empty text.
func messageHasContent(m core.Message) bool {
	if m.Content.IsText() {
		return m.Content.Text != ""
	}
	for _, b := range m.Content.Blocks {
		if b.Type == "text" && b.Text != "" {
			return true
		}
	}
	return false
}

// TruncateText is re-exported from textutil so existing callers keep working.
// The implementation moved because the memory subsystem needs it too, and a
// second copy of a truncation rule is exactly the kind of duplication that has
// already drifted once in this project.
func TruncateText(text string, maxChars int) string {
	return textutil.TruncateText(text, maxChars)
}
