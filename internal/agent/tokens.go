package agent

import (
	"sync"

	"github.com/adrianolimagarcia/nanobot-go/internal/bpe"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// Token estimation, ported from nanobot/utils/helpers.py.
//
// The reference measures prompts with tiktoken's cl100k_base and falls back to
// a UTF-8 byte count when tiktoken cannot be used. Before internal/bpe existed
// this port only had the fallback, and the byte count over-estimates English
// prose by roughly 5x — measured on a real sample, 91 bytes against 18 tokens.
// Because these numbers decide when a conversation is compacted, the port was
// summarising roughly five times too early.
//
// The fallback is kept, and so is its "heuristic" source string: it is what the
// reference reports when tiktoken raises, and the reference's tiktoken DOES
// raise for ordinary user text that happens to contain a special token string,
// because Encoding.encode defaults to disallowed_special="all".
//
// Go symbol -> Python symbol:
//
//	EstimatePromptTokens            estimate_prompt_tokens / _estimate_prompt_tokens_with_source
//	EstimatePromptTokensChain       estimate_prompt_tokens_chain
//	EstimateMessageTokens           estimate_message_tokens
//	estimateToolsTokens             _estimate_tools_tokens
//	promptParts                     the `parts` list of _estimate_prompt_tokens_with_source
//	messageTokensParts              the `parts` list of estimate_message_tokens
//	TruncateTextToTokens            truncate_text_to_tokens
//
// EstimatePromptTokens and EstimateMessageTokens themselves live in
// governance.go next to their callers; this file holds the machinery.

// ---------------------------------------------------------------------------
// _estimate_tools_tokens
// ---------------------------------------------------------------------------

// toolsTokenCacheMaxEntries is _TOOLS_TOKEN_CACHE_MAX_ENTRIES (helpers.py:25).
const toolsTokenCacheMaxEntries = 64

var (
	toolsTokenCacheMu sync.Mutex
	toolsTokenCache   = map[string]int{}
	toolsTokenOrder   []string
)

// estimateToolsTokens is _estimate_tools_tokens (helpers.py:123).
//
// DELIBERATE DIVERGENCE, in the cache KEY only. The reference keys its cache on
// `id(tools)` plus a fingerprint of `id(tool)` for each element, i.e. object
// identity, because re-rendering the JSON on every agent-loop iteration is the
// thing it is avoiding. Go has no stable notion of an object identity that
// survives a slice copy, so this memoises on the rendered JSON instead. The
// rendered string determines the returned count exactly, so the VALUE is
// identical for every input; only the hit rate can differ, and it can only be
// better (a freshly built but identical tools list hits here and misses there).
func estimateToolsTokens(tools []provider.ToolSchema, leadingSeparator bool) (int, error) {
	raw, err := marshalSpaced(tools)
	if err != nil {
		return 0, err
	}
	rendered := string(raw)
	if leadingSeparator {
		rendered = "\n" + rendered
	}

	toolsTokenCacheMu.Lock()
	if n, ok := toolsTokenCache[rendered]; ok {
		toolsTokenCacheMu.Unlock()
		return n, nil
	}
	toolsTokenCacheMu.Unlock()

	// len(enc.encode(rendered)) — this is where a special token in a tool
	// definition would raise and send the whole estimate to the byte fallback.
	n, err := bpe.TokenCount(rendered)
	if err != nil {
		return 0, err
	}

	toolsTokenCacheMu.Lock()
	if _, ok := toolsTokenCache[rendered]; !ok {
		if len(toolsTokenOrder) >= toolsTokenCacheMaxEntries {
			delete(toolsTokenCache, toolsTokenOrder[0])
			toolsTokenOrder = toolsTokenOrder[1:]
		}
		toolsTokenCache[rendered] = n
		toolsTokenOrder = append(toolsTokenOrder, rendered)
	}
	toolsTokenCacheMu.Unlock()
	return n, nil
}

// ---------------------------------------------------------------------------
// _estimate_prompt_tokens_with_source
// ---------------------------------------------------------------------------

// promptParts is the per-message `parts` list of
// _estimate_prompt_tokens_with_source (helpers.py:728-752).
//
// The field ORDER is load-bearing: the parts are joined with "\n" and the
// joined string is what gets encoded, so reordering them changes the token
// count. This order is content, tool_calls, reasoning_content, name,
// tool_call_id — and it is NOT the order estimate_message_tokens uses.
func promptParts(m core.Message) []string {
	var parts []string

	switch {
	case m.Content.IsText():
		// isinstance(content, str) -> appended even when empty.
		parts = append(parts, m.Content.Text)
	case m.Content.Blocks != nil:
		for _, b := range m.Content.Blocks {
			// Only text parts are counted here; non-text parts are skipped,
			// not serialized. (estimate_message_tokens does serialize them.)
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}

	if len(m.ToolCalls) > 0 {
		if raw, err := marshalSpaced(m.ToolCalls); err == nil {
			parts = append(parts, string(raw))
		}
	}
	if m.ReasoningContent != "" {
		parts = append(parts, m.ReasoningContent)
	}
	if m.Name != "" {
		parts = append(parts, m.Name)
	}
	if m.ToolCallID != "" {
		parts = append(parts, m.ToolCallID)
	}
	return parts
}

// estimatePromptTokensWithSource is _estimate_prompt_tokens_with_source
// (helpers.py:719).
func estimatePromptTokensWithSource(messages []core.Message, tools []provider.ToolSchema) (int, string) {
	var parts []string
	for _, m := range messages {
		parts = append(parts, promptParts(m)...)
	}
	messagePayload := joinWithNewline(parts)
	perMessageOverhead := len(messages) * 4

	// try: enc = _get_token_encoding(); tool_tokens = ...; message_tokens = ...
	if bpe.Available() {
		toolTokens := 0
		ok := true
		if len(tools) > 0 {
			n, err := estimateToolsTokens(tools, len(parts) > 0)
			if err != nil {
				ok = false
			} else {
				toolTokens = n
			}
		}
		if ok {
			messageTokens := 0
			if messagePayload != "" {
				n, err := bpe.TokenCount(messagePayload)
				if err != nil {
					ok = false
				} else {
					messageTokens = n
				}
			}
			if ok {
				return messageTokens + toolTokens + perMessageOverhead, "tiktoken"
			}
		}
	}

	// except Exception: the conservative UTF-8 byte budget.
	toolPayload := ""
	if len(tools) > 0 {
		if raw, err := marshalSpaced(tools); err == nil {
			sep := ""
			if messagePayload != "" {
				sep = "\n"
			}
			toolPayload = sep + string(raw)
		}
	}
	return len(messagePayload+toolPayload) + perMessageOverhead, "heuristic"
}

// ---------------------------------------------------------------------------
// estimate_message_tokens
// ---------------------------------------------------------------------------

// messageTokensParts is the `parts` list of estimate_message_tokens
// (helpers.py:783-810).
//
// It differs from promptParts in two ways that both change the count:
//
//   - non-text content blocks ARE serialized with json.dumps, so an image block
//     contributes its JSON rather than nothing;
//   - the order is content, name, tool_call_id, tool_calls, reasoning_content.
//
// A previous revision of this port used the promptParts order for both, which
// produced a different payload string and therefore a different number.
func messageTokensParts(m core.Message) []string {
	var parts []string

	switch {
	case m.Content.IsText():
		parts = append(parts, m.Content.Text)
	case m.Content.Blocks != nil:
		for _, b := range m.Content.Blocks {
			if b.Type == "text" {
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
				continue
			}
			if raw, err := marshalSpaced(b); err == nil {
				parts = append(parts, string(raw))
			}
		}
	}
	// The reference's `elif content is not None: parts.append(json.dumps(...))`
	// arm handles a content value that is neither a string nor a list — a bare
	// number, say. core.Content cannot represent that, so it has no counterpart
	// here; a missing content key leaves Content zero and adds nothing, which is
	// what the reference's `is None` case does.

	for _, key := range [...]string{m.Name, m.ToolCallID} {
		if key != "" {
			parts = append(parts, key)
		}
	}
	if len(m.ToolCalls) > 0 {
		if raw, err := marshalSpaced(m.ToolCalls); err == nil {
			parts = append(parts, string(raw))
		}
	}
	if m.ReasoningContent != "" {
		parts = append(parts, m.ReasoningContent)
	}
	return parts
}

// ---------------------------------------------------------------------------
// estimate_prompt_tokens_chain
// ---------------------------------------------------------------------------

// PromptTokenEstimator is the optional provider-side counter that
// estimate_prompt_tokens_chain probes for with
// `getattr(provider, "estimate_prompt_tokens", None)`.
//
// No provider in the frozen reference defines that attribute — the only two
// definitions of the name in the tree are the two helpers in helpers.py — so in
// practice the first tier is always skipped and the chain is tiktoken then
// heuristic. The interface exists so a provider that wants to supply a real
// count can, exactly as the reference allows.
type PromptTokenEstimator interface {
	EstimatePromptTokens(messages []core.Message, tools []provider.ToolSchema, model string) (int, string)
}

// EstimatePromptTokensChain is estimate_prompt_tokens_chain (helpers.py:822).
//
// The reference wraps the provider call in `contextlib.suppress(Exception)` and
// only accepts a result that is a positive number; anything else falls through
// to tiktoken. In Go that is a recover, not an error check, because the
// reference suppresses panics raised by arbitrary provider code.
func EstimatePromptTokensChain(
	providerCounter any,
	model string,
	messages []core.Message,
	tools []provider.ToolSchema,
) (int, string) {
	if counter, ok := providerCounter.(PromptTokenEstimator); ok && counter != nil {
		if tokens, source, ok := tryProviderCounter(counter, messages, tools, model); ok && tokens > 0 {
			if source == "" {
				source = "provider_counter"
			}
			return tokens, source
		}
	}
	estimated, source := estimatePromptTokensWithSource(messages, tools)
	if estimated > 0 {
		return estimated, source
	}
	return 0, "none"
}

// tryProviderCounter is the `with suppress(Exception)` block. ok is false when
// the provider raised or reported a non-positive count.
func tryProviderCounter(
	counter PromptTokenEstimator,
	messages []core.Message,
	tools []provider.ToolSchema,
	model string,
) (tokens int, source string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			tokens, source, ok = 0, "", false
		}
	}()
	tokens, source = counter.EstimatePromptTokens(messages, tools, model)
	if tokens <= 0 {
		return 0, "", false
	}
	return tokens, source, true
}

// ---------------------------------------------------------------------------
// truncate_text_to_tokens
// ---------------------------------------------------------------------------

// TruncateTextToTokens is nanobot.utils.helpers.truncate_text_to_tokens
// (helpers.py:409), the TOKEN-based truncation.
//
// It is not TruncateText, which is the character-based truncate_text
// (helpers.py:402). memory.py uses the token variant in six places to bound the
// text it stores; the character variant appears nowhere in the memory path.
func TruncateTextToTokens(text string, maxTokens int) string {
	return bpe.TruncateTextToTokens(text, maxTokens)
}
