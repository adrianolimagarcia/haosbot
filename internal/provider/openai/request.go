package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// ---------------------------------------------------------------------------
// Request body
// ---------------------------------------------------------------------------

// chatRequestBody is the Chat Completions request body.
//
// Field order follows the reference's kwargs insertion order
// (openai_compat_provider.py:949-1102, 2124-2126). The reference's `timeout`
// kwarg is deliberately absent: it is an SDK request option, not a body field.
type chatRequestBody struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	Temperature         *float64          `json:"temperature,omitempty"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
	Tools               []map[string]any  `json:"tools,omitempty"`
	ToolChoice          any               `json:"tool_choice,omitempty"`
	Stream              *bool             `json:"stream,omitempty"`
	StreamOptions       *streamOptions    `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// buildBody renders the request JSON.
//
// stream is true only for ChatStream. The reference's non-streaming chat()
// never passes `stream` to the SDK (openai_compat_provider.py:2002-2011), and
// the SDK drops omitted parameters from the body entirely
// (SDK: openai/_utils/_transform.py:269-272), so the non-streaming body has no
// `stream` key at all — servers treat that as stream=false.
func (c *Client) buildBody(req provider.ChatRequest, stream bool) ([]byte, error) {
	req.ApplyDefaults()

	model := req.Model
	if model == "" {
		model = c.model
	}

	messages, err := sanitizeMessages(req.Messages, model, req.ReasoningEffort)
	if err != nil {
		return nil, err
	}

	body := chatRequestBody{Model: model, Messages: messages}

	// GPT-5 and o-series models reject temperature when reasoning is active
	// (openai_compat_provider.py:957-960, 896-912).
	if supportsTemperature(model, req.ReasoningEffort) {
		temp := req.Temperature
		body.Temperature = &temp
	}

	// `max(1, max_tokens)` (openai_compat_provider.py:962-967).
	maxTokens := req.MaxTokens
	if maxTokens < 1 {
		maxTokens = 1
	}
	if requiresMaxCompletionTokens(model) {
		body.MaxCompletionTokens = &maxTokens
	} else {
		body.MaxTokens = &maxTokens
	}

	body.ReasoningEffort = wireReasoningEffort(model, req.ReasoningEffort)

	// Tools are sent only when non-empty, with tool_choice defaulting to
	// "auto" (openai_compat_provider.py:1069-1071).
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, t.OpenAITool())
		}
		body.Tools = tools
		choice := req.ToolChoice
		if choice == nil {
			choice = "auto"
		}
		body.ToolChoice = choice
	}

	if stream {
		enabled := true
		body.Stream = &enabled
		// kwargs["stream_options"] = {"include_usage": True}
		// (openai_compat_provider.py:2126).
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	return marshalNoHTMLEscape(body)
}

// ---------------------------------------------------------------------------
// Model capability rules (all spec-independent)
// ---------------------------------------------------------------------------

// modelSlug mirrors _model_slug (openai_compat_provider.py:168): lowercase,
// last path segment.
func modelSlug(model string) string {
	lowered := strings.ToLower(model)
	if idx := strings.LastIndex(lowered, "/"); idx >= 0 {
		return lowered[idx+1:]
	}
	return lowered
}

// supportsTemperature mirrors _supports_temperature
// (openai_compat_provider.py:896-912).
func supportsTemperature(model, reasoningEffort string) bool {
	slug := modelSlug(model)
	if slug == "kimi-k3" {
		return false
	}
	if reasoningEffort != "" && strings.ToLower(reasoningEffort) != "none" {
		return false
	}
	name := strings.ToLower(model)
	for _, token := range []string{"gpt-5", "o1", "o3", "o4"} {
		if strings.Contains(name, token) {
			return false
		}
	}
	return true
}

// requiresMaxCompletionTokens mirrors _requires_max_completion_tokens
// (openai_compat_provider.py:176-181).
func requiresMaxCompletionTokens(model string) bool {
	slug := modelSlug(model)
	if slug == "kimi-k3" || strings.Contains(slug, "gpt-5") {
		return true
	}
	for _, prefix := range []string{"o1", "o3", "o4"} {
		if slug == prefix || strings.HasPrefix(slug, prefix+"-") || strings.HasPrefix(slug, prefix+".") {
			return true
		}
	}
	return false
}

// wireReasoningEffort returns the reasoning_effort value to send, or "" to omit
// it. Mirrors the spec-independent half of _build_kwargs
// (openai_compat_provider.py:986-1039): "minimum" is a DashScope alias for
// "minimal", kimi-k3 only accepts "max" and omits disabled/default reasoning,
// and "none" is never sent.
func wireReasoningEffort(model, reasoningEffort string) string {
	if reasoningEffort == "" {
		return ""
	}
	semantic := strings.ToLower(reasoningEffort)
	if semantic == "minimum" {
		semantic = "minimal"
	}
	wire := reasoningEffort
	if modelSlug(model) == "kimi-k3" {
		if semantic == "none" || semantic == "minimal" {
			return ""
		}
		return "max"
	}
	if semantic == "none" {
		return ""
	}
	return wire
}

// thinkingStyleModels are the model slugs whose native thinking control is
// enabled through reasoning_effort alone (_KIMI_THINKING_MODELS,
// _MIMO_THINKING_MODELS, _QWEN_THINKING_MODELS — openai_compat_provider.py:103-165).
var thinkingStyleModels = map[string]bool{
	"kimi-k2.5": true, "kimi-k2.6": true, "kimi-k2.7": true,
	"kimi-k2.7-code": true, "kimi-k2.7-code-highspeed": true, "k2.6-code-preview": true,
	"mimo-v2.5-pro": true, "mimo-v2.5": true, "mimo-v2-pro": true, "mimo-v2-omni": true,
	"qwen3.7-max": true, "qwen3.7-plus": true, "qwen3.6-max-preview": true,
	"qwen3.6-plus": true, "qwen3.6-flash": true, "qwen3.5-plus": true, "qwen3.5-flash": true,
}

// shouldBackfillReasoning mirrors the `explicit_thinking` half of
// openai_compat_provider.py:1077-1094: when reasoning is explicitly enabled for
// a model with a native thinking control, every assistant message that lacks
// reasoning_content gets "" so the provider does not reject the history.
func shouldBackfillReasoning(model, reasoningEffort string) bool {
	if reasoningEffort == "" {
		return false
	}
	semantic := strings.ToLower(reasoningEffort)
	if semantic == "minimum" {
		semantic = "minimal"
	}
	if semantic == "none" || semantic == "minimal" {
		return false
	}
	return thinkingStyleModels[modelSlug(model)]
}

// ---------------------------------------------------------------------------
// Message serialization
// ---------------------------------------------------------------------------

// allowedMessageKeys mirrors _ALLOWED_MSG_KEYS (openai_compat_provider.py:89-92).
// Everything else — including the transcript-only "timestamp", "thinking_blocks"
// and "_hidden_history" — is stripped before the request leaves the process.
var allowedMessageKeys = map[string]bool{
	"role":              true,
	"content":           true,
	"tool_calls":        true,
	"tool_call_id":      true,
	"name":              true,
	"reasoning_content": true,
	"extra_content":     true,
}

// wireMessage is one outgoing message: an ordered list of JSON fields, so the
// emitted object keeps the reference's key order.
type wireMessage struct {
	fields []wireField
}

type wireField struct {
	key string
	raw json.RawMessage
}

func (m *wireMessage) get(key string) (json.RawMessage, bool) {
	for _, f := range m.fields {
		if f.key == key {
			return f.raw, true
		}
	}
	return nil, false
}

func (m *wireMessage) set(key string, raw json.RawMessage) {
	for i := range m.fields {
		if m.fields[i].key == key {
			m.fields[i].raw = raw
			return
		}
	}
	m.fields = append(m.fields, wireField{key: key, raw: raw})
}

func (m *wireMessage) clone() *wireMessage {
	cp := &wireMessage{fields: make([]wireField, len(m.fields))}
	copy(cp.fields, m.fields)
	return cp
}

func (m *wireMessage) role() string {
	raw, ok := m.get("role")
	if !ok {
		return ""
	}
	return jsonString(raw)
}

// hasToolCalls reports whether the message carries a non-empty tool_calls list.
func (m *wireMessage) hasToolCalls() bool {
	raw, ok := m.get("tool_calls")
	if !ok {
		return false
	}
	var calls []json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return false
	}
	return len(calls) > 0
}

// contentString returns the content when it is a JSON string.
func (m *wireMessage) contentString() *string {
	raw, ok := m.get("content")
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil
	}
	return &s
}

func (m *wireMessage) marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range m.fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalNoHTMLEscape(f.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(f.raw)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// sanitizeMessages reproduces the reference pipeline
// `_sanitize_messages(_sanitize_empty_content(messages))`
// (openai_compat_provider.py:951-955): strip non-API keys, repair empty
// content, rewrite tool calls into wire shape, and enforce role alternation.
func sanitizeMessages(messages []core.Message, model, reasoningEffort string) ([]json.RawMessage, error) {
	sanitized := make([]*wireMessage, 0, len(messages))
	for i := range messages {
		// core.Message.MarshalJSON emits role, content, timestamp, ... in the
		// reference's key order; the pointer receiver is required for it to run.
		raw, err := json.Marshal(&messages[i])
		if err != nil {
			return nil, fmt.Errorf("openai: marshal message %d: %w", i, err)
		}
		fields, err := decodeOrderedFields(raw)
		if err != nil {
			return nil, fmt.Errorf("openai: decode message %d: %w", i, err)
		}
		msg := &wireMessage{}
		for _, f := range fields {
			if allowedMessageKeys[f.key] {
				msg.fields = append(msg.fields, f)
			}
		}
		if err := normalizeContent(msg); err != nil {
			return nil, fmt.Errorf("openai: normalize message %d content: %w", i, err)
		}
		if err := normalizeToolCalls(msg); err != nil {
			return nil, fmt.Errorf("openai: normalize message %d tool_calls: %w", i, err)
		}
		sanitized = append(sanitized, msg)
	}

	merged := enforceRoleAlternation(sanitized)

	if shouldBackfillReasoning(model, reasoningEffort) {
		for _, msg := range merged {
			if msg.role() == string(core.RoleAssistant) {
				if _, ok := msg.get("reasoning_content"); !ok {
					msg.set("reasoning_content", json.RawMessage(`""`))
				}
			}
		}
	}

	out := make([]json.RawMessage, 0, len(merged))
	for _, msg := range merged {
		raw, err := msg.marshal()
		if err != nil {
			return nil, fmt.Errorf("openai: marshal sanitized message: %w", err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// decodeOrderedFields splits a JSON object into ordered key/value pairs.
func decodeOrderedFields(data []byte) ([]wireField, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var fields []wireField
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("object key must be a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		fields = append(fields, wireField{key: key, raw: raw})
	}
	return fields, nil
}

// normalizeContent mirrors LLMProvider._sanitize_empty_content
// (providers/base.py:813-873) for the representations core.Content can produce.
//
// NOTE: the reference additionally scrubs lone UTF-16 surrogates
// (sanitize_surrogates_deep). Go strings are UTF-8 and encoding/json already
// replaces invalid bytes with U+FFFD while encoding, so no extra pass is
// needed here.
func normalizeContent(m *wireMessage) error {
	raw, ok := m.get("content")
	if !ok {
		// _sanitize_request_messages forces content=None on assistant messages
		// that have no content key (base.py:917-918).
		if m.role() == string(core.RoleAssistant) {
			m.set("content", json.RawMessage("null"))
		}
		return nil
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		if s == "" {
			m.set("content", json.RawMessage(emptyContentLiteral(m)))
		}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		kept := make([]json.RawMessage, 0, len(items))
		changed := false
		for _, item := range items {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(item, &obj); err != nil {
				kept = append(kept, item)
				continue
			}
			blockType := jsonString(obj["type"])
			if (blockType == "text" || blockType == "input_text" || blockType == "output_text") &&
				!truthy(obj["text"]) {
				changed = true
				continue
			}
			if _, hasMeta := obj["_meta"]; hasMeta {
				delete(obj, "_meta")
				rebuilt, err := marshalNoHTMLEscape(obj)
				if err != nil {
					return err
				}
				kept = append(kept, rebuilt)
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		if !changed {
			return nil
		}
		if len(kept) == 0 {
			m.set("content", json.RawMessage(emptyContentLiteral(m)))
			return nil
		}
		rebuilt, err := marshalNoHTMLEscape(kept)
		if err != nil {
			return err
		}
		m.set("content", rebuilt)
	case '{':
		// Python wraps bare dict content in a one-element list (base.py:863-867).
		m.set("content", json.RawMessage("["+string(trimmed)+"]"))
	}
	return nil
}

// emptyContentLiteral returns the replacement for empty content: null for an
// assistant message that carries tool calls, "(empty)" otherwise
// (base.py:829-861).
func emptyContentLiteral(m *wireMessage) string {
	if m.role() == string(core.RoleAssistant) && m.hasToolCalls() {
		return "null"
	}
	return `"(empty)"`
}

// normalizeToolCalls rewrites the transcript's flattened tool calls into the
// OpenAI wire shape {"id","type","function":{"name","arguments"}} and forces
// function.arguments to be a JSON *string* (base.py:86-107,
// openai_compat_provider.py:776-785).
func normalizeToolCalls(m *wireMessage) error {
	raw, ok := m.get("tool_calls")
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		// The reference leaves non-dict entries untouched; a shape this far
		// off is passed through rather than silently rewritten.
		return nil
	}

	calls := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		call := map[string]json.RawMessage{}
		id, ok := item["id"]
		if !ok || !truthy(id) {
			id = json.RawMessage(`""`)
		}
		call["id"] = id
		call["type"] = json.RawMessage(`"function"`)

		function := map[string]json.RawMessage{}
		name, ok := item["name"]
		if !ok {
			name = json.RawMessage(`""`)
		}
		function["name"] = name

		args, err := toolArgumentsJSONForReplay(item["arguments"])
		if err != nil {
			return err
		}
		function["arguments"] = args
		if fnProv, ok := item["function_provider_specific_fields"]; ok && truthy(fnProv) {
			function["provider_specific_fields"] = fnProv
		}
		encodedFunction, err := marshalNoHTMLEscape(function)
		if err != nil {
			return err
		}
		call["function"] = encodedFunction

		if extra, ok := item["extra_content"]; ok && truthy(extra) {
			call["extra_content"] = extra
		}
		if prov, ok := item["provider_specific_fields"]; ok && truthy(prov) {
			call["provider_specific_fields"] = prov
		}
		encoded, err := marshalNoHTMLEscape(call)
		if err != nil {
			return err
		}
		calls = append(calls, encoded)
	}

	encodedCalls, err := marshalNoHTMLEscape(calls)
	if err != nil {
		return err
	}
	m.set("tool_calls", encodedCalls)
	return nil
}

// toolArgumentsJSONForReplay mirrors tool_arguments_json_for_replay
// (base.py:133-163): the result is always a JSON string holding a JSON object,
// defaulting to "{}".
//
// DIVERGENCE (UNVERIFIED): the reference repairs malformed argument text with
// the third-party `json_repair` package before falling back to "{}". This port
// has no dependency-free equivalent, so malformed arguments become "{}" here.
func toolArgumentsJSONForReplay(raw json.RawMessage) (json.RawMessage, error) {
	obj := json.RawMessage(`{}`)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 {
		var value any
		if err := json.Unmarshal(trimmed, &value); err == nil {
			switch typed := value.(type) {
			case map[string]any:
				encoded, err := marshalNoHTMLEscape(typed)
				if err != nil {
					return nil, err
				}
				obj = encoded
			case string:
				// ToolCall.Arguments may already be a JSON string.
				var inner any
				if err := json.Unmarshal([]byte(typed), &inner); err == nil {
					if nested, ok := inner.(map[string]any); ok {
						encoded, err := marshalNoHTMLEscape(nested)
						if err != nil {
							return nil, err
						}
						obj = encoded
					}
				}
			}
		}
	}
	return marshalNoHTMLEscape(string(obj))
}

// enforceRoleAlternation mirrors LLMProvider._enforce_role_alternation
// (base.py:1123-1197): merge consecutive same-role user/assistant turns, drop
// trailing assistant messages, and guarantee the first non-system message is
// not a bare assistant message.
func enforceRoleAlternation(messages []*wireMessage) []*wireMessage {
	if len(messages) == 0 {
		return messages
	}

	merged := make([]*wireMessage, 0, len(messages))
	for _, msg := range messages {
		role := msg.role()
		mergeable := role == string(core.RoleUser) || role == string(core.RoleAssistant)
		if len(merged) > 0 && mergeable && merged[len(merged)-1].role() == role {
			prev := merged[len(merged)-1]
			if role == string(core.RoleAssistant) {
				switch {
				case msg.hasToolCalls():
					merged[len(merged)-1] = msg
					continue
				case prev.hasToolCalls():
					continue
				}
			}
			prevContent := prev.contentString()
			currContent := msg.contentString()
			if prevContent != nil && currContent != nil {
				prev.set("content", mustJSONString(strings.TrimSpace(*prevContent+"\n\n"+*currContent)))
				continue
			}
			if role == string(core.RoleUser) {
				combined := contentBlocks(prev)
				combined = append(combined, contentBlocks(msg)...)
				encoded, err := marshalNoHTMLEscape(combined)
				if err != nil {
					encoded = json.RawMessage(`"(empty)"`)
				}
				replacement := msg.clone()
				replacement.set("content", encoded)
				merged[len(merged)-1] = replacement
				continue
			}
			merged[len(merged)-1] = msg
			continue
		}
		merged = append(merged, msg)
	}

	// Providers such as vLLM and Ollama reject a trailing assistant message
	// (prefill is not supported).
	var lastPopped *wireMessage
	for len(merged) > 0 && merged[len(merged)-1].role() == string(core.RoleAssistant) {
		lastPopped = merged[len(merged)-1]
		merged = merged[:len(merged)-1]
	}

	// Recover when that left only system messages, otherwise the request is
	// invalid for providers like Zhipu/GLM (error 1214).
	if len(merged) > 0 && lastPopped != nil && !hasRole(merged, core.RoleUser) && !hasRole(merged, core.RoleTool) {
		recovered := lastPopped.clone()
		recovered.set("role", mustJSONString(string(core.RoleUser)))
		merged = append(merged, recovered)
	}

	// A system → assistant start is rejected by the same providers.
	for i, msg := range merged {
		if msg.role() == string(core.RoleSystem) {
			continue
		}
		if msg.role() == string(core.RoleAssistant) && !msg.hasToolCalls() {
			synthetic := &wireMessage{}
			synthetic.set("role", mustJSONString(string(core.RoleUser)))
			synthetic.set("content", mustJSONString(syntheticUserContent))
			merged = append(merged[:i], append([]*wireMessage{synthetic}, merged[i:]...)...)
		}
		break
	}

	return merged
}

// syntheticUserContent mirrors _SYNTHETIC_USER_CONTENT (base.py:620).
const syntheticUserContent = "(conversation continued)"

func hasRole(messages []*wireMessage, role core.Role) bool {
	for _, msg := range messages {
		if msg.role() == string(role) {
			return true
		}
	}
	return false
}

// contentBlocks mirrors _content_as_blocks (base.py:1109-1121).
func contentBlocks(m *wireMessage) []json.RawMessage {
	raw, ok := m.get("content")
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil
		}
		out := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(item, &obj); err == nil && obj != nil {
				out = append(out, item)
				continue
			}
			out = append(out, textBlock(jsonString(item)))
		}
		return out
	}
	if trimmed[0] == '"' {
		return []json.RawMessage{textBlock(jsonString(trimmed))}
	}
	// Python str()s any other scalar into a text block.
	return []json.RawMessage{textBlock(string(trimmed))}
}

func textBlock(text string) json.RawMessage {
	encoded, err := marshalNoHTMLEscape(map[string]any{"type": "text", "text": text})
	if err != nil {
		return json.RawMessage(`{"type":"text","text":""}`)
	}
	return encoded
}

// ---------------------------------------------------------------------------
// small JSON helpers
// ---------------------------------------------------------------------------

// jsonString decodes a JSON string value, returning "" for anything else.
func jsonString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return ""
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return ""
	}
	return s
}

// isJSONString reports whether raw is a JSON string value.
func isJSONString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return false
	}
	var s string
	return json.Unmarshal(trimmed, &s) == nil
}

// truthy mirrors Python truthiness for the JSON values the reference tests with
// `if value:`.
func truthy(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch string(trimmed) {
	case "null", "false", `""`, "{}", "[]", "0":
		return false
	}
	if trimmed[0] == '"' {
		return jsonString(trimmed) != ""
	}
	return true
}

func mustJSONString(s string) json.RawMessage {
	encoded, err := marshalNoHTMLEscape(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}
