package openai

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

type wireAssistantMessage struct {
	Content          json.RawMessage   `json:"content"`
	ReasoningContent json.RawMessage   `json:"reasoning_content"`
	Reasoning        json.RawMessage   `json:"reasoning"`
	ToolCalls        []json.RawMessage `json:"tool_calls"`
}

type wireChoice struct {
	Message      wireAssistantMessage `json:"message"`
	FinishReason *string              `json:"finish_reason"`
}

type wireResponse struct {
	Choices          []wireChoice    `json:"choices"`
	Usage            json.RawMessage `json:"usage"`
	Content          json.RawMessage `json:"content"`
	OutputText       json.RawMessage `json:"output_text"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	FinishReason     *string         `json:"finish_reason"`
}

// ---------------------------------------------------------------------------
// Non-streaming parse
// ---------------------------------------------------------------------------

// parseResponse mirrors OpenAICompatProvider._parse
// (openai_compat_provider.py:1542-1686) for a decoded JSON response.
func parseResponse(payload []byte) (*core.Response, error) {
	var resp wireResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}

	if len(resp.Choices) == 0 {
		// Some gateways answer with a bare content/output_text payload
		// (openai_compat_provider.py:1552-1565).
		if raw := firstTruthyRaw(resp.Content, resp.OutputText); raw != nil {
			content := extractTextContent(raw)
			content, calls := extractTextToolCalls(content)
			if content != "" || len(calls) > 0 {
				finish := "stop"
				if resp.FinishReason != nil && *resp.FinishReason != "" {
					finish = *resp.FinishReason
				}
				return &core.Response{
					Content:          content,
					HasContent:       content != "",
					ToolCalls:        calls,
					FinishReason:     core.FinishReason(finish),
					Usage:            extractUsage(resp.Usage),
					ReasoningContent: extractTextContent(resp.ReasoningContent),
				}, nil
			}
		}
		return &core.Response{
			Content:      "Error: API returned empty choices.",
			FinishReason: core.FinishError,
			ErrorKind:    "empty",
		}, nil
	}

	choice0 := resp.Choices[0]
	content := extractTextContent(choice0.Message.Content)
	finish := "stop"
	if choice0.FinishReason != nil && *choice0.FinishReason != "" {
		finish = *choice0.FinishReason
	}

	reasoning := ""
	if isJSONString(choice0.Message.ReasoningContent) {
		reasoning = jsonString(choice0.Message.ReasoningContent)
	} else if truthy(choice0.Message.Reasoning) {
		reasoning = extractTextContent(choice0.Message.Reasoning)
	}

	// Every choice is scanned for tool calls, and a tool-capable finish reason
	// from any choice wins (openai_compat_provider.py:1590-1603).
	var rawCalls []json.RawMessage
	for _, choice := range resp.Choices {
		if len(choice.Message.ToolCalls) > 0 {
			rawCalls = append(rawCalls, choice.Message.ToolCalls...)
			if choice.FinishReason != nil {
				switch *choice.FinishReason {
				case "tool_calls", "stop":
					finish = *choice.FinishReason
				}
			}
		}
		if content == "" {
			content = extractTextContent(choice.Message.Content)
		}
		if reasoning == "" && isJSONString(choice.Message.ReasoningContent) {
			reasoning = jsonString(choice.Message.ReasoningContent)
		}
	}

	// Empty or duplicated tool-call ids get a fresh short id so replayed tool
	// results cannot collide (openai_compat_provider.py:1605-1625).
	seen := make(map[string]bool, len(rawCalls))
	calls := make([]core.ToolCall, 0, len(rawCalls))
	for _, raw := range rawCalls {
		call := parseToolCall(raw)
		if call.ID == "" || seen[call.ID] {
			call.ID = shortToolID()
		}
		seen[call.ID] = true
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		content, calls = extractTextToolCalls(content)
	}

	return &core.Response{
		Content:          content,
		HasContent:       content != "",
		ToolCalls:        calls,
		FinishReason:     core.FinishReason(finish),
		Usage:            extractUsage(resp.Usage),
		ReasoningContent: reasoning,
	}, nil
}

// firstTruthyRaw returns the first value that is present and truthy, mirroring
// Python's `a or b`.
func firstTruthyRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if truthy(v) {
			return v
		}
	}
	return nil
}

// standardToolCallKeys and standardFunctionKeys mirror _STANDARD_TC_KEYS and
// _STANDARD_FN_KEYS (openai_compat_provider.py:95-96).
var (
	standardToolCallKeys = map[string]bool{"id": true, "type": true, "index": true, "function": true}
	standardFunctionKeys = map[string]bool{"name": true, "arguments": true}
)

// parseToolCall mirrors the tool-call branch of _parse plus _extract_tc_extras
// (openai_compat_provider.py:319-351, 1609-1625).
func parseToolCall(raw json.RawMessage) core.ToolCall {
	var call map[string]json.RawMessage
	_ = json.Unmarshal(raw, &call)

	function := map[string]json.RawMessage{}
	if fn, ok := call["function"]; ok {
		_ = json.Unmarshal(fn, &function)
	}

	return core.ToolCall{
		ID:                       jsonString(call["id"]),
		Name:                     jsonString(function["name"]),
		Arguments:                parseToolArguments(function["arguments"]),
		ExtraContent:             jsonObject(call["extra_content"]),
		ProviderSpecificFields:   jsonLeftovers(call, standardToolCallKeys, "extra_content"),
		FunctionProviderSpecific: jsonLeftovers(function, standardFunctionKeys),
	}
}

// jsonLeftovers returns the non-standard keys of obj whose value is not null.
func jsonLeftovers(obj map[string]json.RawMessage, exclude map[string]bool, alsoExclude ...string) map[string]any {
	var out map[string]any
	for key, value := range obj {
		if exclude[key] {
			continue
		}
		skip := false
		for _, extra := range alsoExclude {
			if key == extra {
				skip = true
				break
			}
		}
		if skip || !truthyOrZero(value) {
			continue
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[key] = decoded
	}
	return out
}

// truthyOrZero reports whether a value is present and not JSON null, matching
// the reference's `v is not None` filter.
func truthyOrZero(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null"
}

// jsonObject decodes a JSON object, returning nil when it is absent or empty
// (mirrors _coerce_dict, openai_compat_provider.py:305-316).
func jsonObject(raw json.RawMessage) map[string]any {
	if !truthyOrZero(raw) {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Content extraction
// ---------------------------------------------------------------------------

// extractTextContent mirrors _extract_text_content
// (openai_compat_provider.py:1410-1436): a string passes through, a list of
// blocks contributes its text parts (Mistral-style "thinking" blocks excluded),
// and any other scalar is stringified.
func extractTextContent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	switch trimmed[0] {
	case '"':
		return jsonString(trimmed)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return ""
		}
		var builder strings.Builder
		for _, item := range items {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(item, &obj); err == nil && obj != nil {
				if jsonString(obj["type"]) == "thinking" {
					continue
				}
				if text, ok := obj["text"]; ok && isJSONString(text) {
					builder.WriteString(jsonString(text))
					continue
				}
			}
			if isJSONString(item) {
				builder.WriteString(jsonString(item))
			}
		}
		return builder.String()
	default:
		return string(trimmed)
	}
}

// extractThinkingContent mirrors _extract_thinking_content
// (openai_compat_provider.py:1438-1461): Mistral returns thinking text inside
// the content array as {"type":"thinking","thinking":[...]}.
func extractThinkingContent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ""
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil || obj == nil {
			continue
		}
		if jsonString(obj["type"]) != "thinking" {
			continue
		}
		builder.WriteString(extractTextContent(obj["thinking"]))
	}
	return builder.String()
}

// ---------------------------------------------------------------------------
// Tool arguments
// ---------------------------------------------------------------------------

// parseToolArguments mirrors parse_tool_arguments (providers/base.py:110-130).
//
// The result is raw JSON for the Go core type:
//
//   - absent or JSON null become {}
//   - a JSON string holding valid non-null JSON becomes that JSON
//   - anything else (malformed text, arrays, scalars) is preserved verbatim so
//     the tool registry rejects it instead of executing a guessed call
//
// DIVERGENCE (UNVERIFIED): for malformed text the reference keeps the original
// Go string; this keeps the original JSON string value, which is valid JSON and
// is rejected by tools.NormalizeArguments exactly like the reference's value is.
func parseToolArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{}`)
	}
	if trimmed[0] != '"' {
		return json.RawMessage(trimmed)
	}
	text := jsonString(trimmed)
	if strings.TrimSpace(text) == "" {
		return json.RawMessage(`{}`)
	}
	inner := strings.TrimSpace(text)
	if inner == "null" || !json.Valid([]byte(inner)) {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage(inner)
}

// textToolCallRe mirrors _TEXT_TOOL_CALL_RE (openai_compat_provider.py:122).
var textToolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// textToolCallXMLRe accepts the XML-like function/parameter format emitted by
// some OpenAI-compatible models:
//
//	<tool_call>
//	<function=exec>
//	<parameter=command>printf 'hello'</parameter>
//	</function>
//	</tool_call>
//
// It is intentionally narrower than a general XML parser: tool and parameter
// names must be identifiers, and the closing tags must be present. This keeps
// malformed model output as visible assistant text instead of executing a
// partial or guessed call.
var textToolCallXMLRe = regexp.MustCompile(`(?s)<tool_call>\s*<function\s*=\s*([A-Za-z_][A-Za-z0-9_.:-]*)\s*>(.*?)</function\s*>\s*</tool_call\s*>`)
var textToolCallParameterRe = regexp.MustCompile(`(?s)<parameter\s*=\s*([A-Za-z_][A-Za-z0-9_.:-]*)\s*>(.*?)</parameter\s*>`)

// extractTextToolCalls mirrors _extract_text_tool_calls
// (openai_compat_provider.py:245-295): providers that answer with text-format
// <tool_call> blocks are normalized into structured calls and the matched spans
// are removed from the visible content.
func extractTextToolCalls(content string) (string, []core.ToolCall) {
	if content == "" || !strings.Contains(content, "<tool_call>") {
		return content, nil
	}

	type parsedCall struct {
		call       core.ToolCall
		start, end int
	}
	var parsed []parsedCall

	// JSON payloads are the existing format and remain the first-class path.
	for _, match := range textToolCallRe.FindAllStringSubmatchIndex(content, -1) {
		payloadText := stripJSONFence(content[match[2]:match[3]])
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil || payload == nil {
			continue
		}
		if nested, ok := payload["tool_call"]; ok {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(nested, &inner); err == nil && inner != nil {
				payload = inner
			}
		}
		function := payload
		if fn, ok := payload["function"]; ok {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(fn, &inner); err == nil && inner != nil {
				function = inner
			}
		}
		name := jsonString(function["name"])
		if name == "" {
			continue
		}

		arguments, ok := function["arguments"]
		if !ok {
			arguments = payload["arguments"]
		}
		encodedArgs, err := marshalNoHTMLEscape(rawJSONValue(arguments))
		if err != nil {
			continue
		}

		id := rawScalarString(payload["id"])
		if id == "" {
			id = shortToolID()
		}

		parsed = append(parsed, parsedCall{
			call: core.ToolCall{
				ID:        id,
				Name:      name,
				Arguments: parseToolArguments(encodedArgs),
			},
			start: match[0],
			end:   match[1],
		})
	}

	// Some models emit a non-XML, XML-like format with one text parameter per
	// argument. Convert every parameter to a JSON string value. Values are
	// trimmed only at the boundary so indentation around the tags does not
	// become part of a shell command; internal whitespace and newlines survive.
	for _, match := range textToolCallXMLRe.FindAllStringSubmatchIndex(content, -1) {
		call, ok := parseXMLTextToolCall(
			content[match[2]:match[3]],
			content[match[4]:match[5]],
		)
		if !ok {
			continue
		}
		parsed = append(parsed, parsedCall{call: call, start: match[0], end: match[1]})
	}

	if len(parsed) == 0 {
		return content, nil
	}

	// JSON and XML-like blocks can be interleaved. Sort before removing spans
	// so visible content and tool-call order remain deterministic.
	sort.SliceStable(parsed, func(i, j int) bool {
		return parsed[i].start < parsed[j].start
	})

	calls := make([]core.ToolCall, 0, len(parsed))
	var visible strings.Builder
	last := 0
	for _, item := range parsed {
		// Do not remove an overlapping span twice. This is defensive for future
		// grammar extensions and prevents slicing backwards on malformed input.
		if item.start < last {
			continue
		}
		visible.WriteString(content[last:item.start])
		last = item.end
		calls = append(calls, item.call)
	}
	visible.WriteString(content[last:])
	return strings.TrimSpace(visible.String()), calls
}

// parseXMLTextToolCall converts the XML-like function/parameter block into a
// regular structured call. Any non-whitespace text outside parameter elements
// or duplicate parameter name invalidates the whole block.
func parseXMLTextToolCall(name, body string) (core.ToolCall, bool) {
	params := make(map[string]string)
	last := 0
	for _, match := range textToolCallParameterRe.FindAllStringSubmatchIndex(body, -1) {
		if strings.TrimSpace(body[last:match[0]]) != "" {
			return core.ToolCall{}, false
		}
		key := body[match[2]:match[3]]
		if _, exists := params[key]; exists {
			return core.ToolCall{}, false
		}
		params[key] = html.UnescapeString(strings.TrimSpace(body[match[4]:match[5]]))
		last = match[1]
	}
	if strings.TrimSpace(body[last:]) != "" {
		return core.ToolCall{}, false
	}

	encodedArgs, err := marshalNoHTMLEscape(params)
	if err != nil {
		return core.ToolCall{}, false
	}
	return core.ToolCall{
		ID:        shortToolID(),
		Name:      name,
		Arguments: parseToolArguments(encodedArgs),
	}, true
}

// stripJSONFence mirrors _strip_json_fence (openai_compat_provider.py:235-242).
func stripJSONFence(text string) string {
	stripped := strings.TrimSpace(text)
	if !strings.HasPrefix(stripped, "```") || !strings.HasSuffix(stripped, "```") {
		return stripped
	}
	lines := strings.Split(stripped, "\n")
	if len(lines) < 2 {
		return stripped
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

// rawJSONValue decodes raw JSON into the Go value used when re-encoding text
// tool-call arguments, so a JSON string argument is parsed the same way Python's
// parse_tool_arguments would parse it.
func rawJSONValue(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

// rawScalarString mirrors Python's str(value or "") for a JSON scalar.
func rawScalarString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		return jsonString(trimmed)
	}
	return string(trimmed)
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// extractUsage mirrors _extract_usage (openai_compat_provider.py:1463-1518).
//
// Cached tokens are normalized across providers in the reference's priority
// order: prompt_tokens_details.cached_tokens, then a top-level cached_tokens,
// then DeepSeek/SiliconFlow's prompt_cache_hit_tokens.
//
// EXTENSION (not in the reference): reasoning tokens are read from
// completion_tokens_details.reasoning_tokens, because core.Usage models that
// field and OpenAI/DeepSeek report it there. The reference's LLMUsage has no
// equivalent counter and drops it.
func extractUsage(raw json.RawMessage) *core.Usage {
	if !truthyOrZero(raw) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil
	}

	usage := &core.Usage{
		PromptTokens:     intOrZero(obj["prompt_tokens"]),
		CompletionTokens: intOrZero(obj["completion_tokens"]),
	}
	// LLMUsage.reported guarantees total_tokens >= input + output
	// (providers/base.py:340-357).
	visible := usage.PromptTokens + usage.CompletionTokens
	usage.TotalTokens = visible
	if wireTotal := nestedInt(obj, "total_tokens"); wireTotal != nil && *wireTotal > visible {
		usage.TotalTokens = *wireTotal
	}
	for _, path := range [][]string{
		{"prompt_tokens_details", "cached_tokens"},
		{"cached_tokens"},
		{"prompt_cache_hit_tokens"},
	} {
		if cached := nestedInt(obj, path...); cached != nil {
			usage.CachedTokens = cached
			break
		}
	}
	usage.ReasoningTokens = nestedInt(obj, "completion_tokens_details", "reasoning_tokens")
	return usage
}

// nestedInt mirrors _get_nested_int (openai_compat_provider.py:1520-1540): an
// explicit zero is preserved and booleans count as absent.
func nestedInt(obj map[string]any, path ...string) *int {
	var current any = obj
	for _, segment := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		value, ok := mapping[segment]
		if !ok {
			return nil
		}
		current = value
	}
	switch typed := current.(type) {
	case nil, bool:
		return nil
	case json.Number:
		f, err := typed.Float64()
		if err != nil {
			return nil
		}
		i := int(f)
		return &i
	case string:
		// Python: int(current), with (TypeError, ValueError) -> None.
		// PyInt rather than PyAtoi, because the reference passes the raw value:
		// int() tolerates a narrower whitespace set than str.strip(), so
		// int("5\x1c") raises while int("5\x1c".strip()) is 5.
		i, ok := textutil.PyInt(typed)
		if !ok {
			return nil
		}
		return &i
	}
	return nil
}

// intOrZero mirrors Python's `int(value or 0)` for token counters.
//
// The bool arm is not decoration: `int(True or 0)` is 1 in Python, because the
// truthiness test passes the boolean straight through to int(). A JSON `true`
// in a usage field is malformed input, but reporting 0 where the reference
// reports 1 would be a silent divergence in token accounting.
//
// DELIBERATE DIVERGENCE: the reference raises ValueError out of _parse for any
// non-empty string that int() cannot parse. The expression is
// `int(value or 0)`, and every non-empty string is truthy, so the value is
// handed to int() unchanged and int("   "), int("abc") and int("1.5") all
// raise uncaught. Only "" short-circuits through `or 0`. Measured against
// Python 3.14: int("" or 0) is 0 while int("   " or 0) raises. Reproducing the
// raise would turn one malformed usage field into a failed request, so this
// port reports 0 for every unparseable string. All parseable shapes match.
func intOrZero(value any) int {
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1
		}
		return 0
	case json.Number:
		f, err := typed.Float64()
		if err != nil {
			return 0
		}
		return int(f)
	case string:
		i, ok := textutil.PyInt(typed)
		if !ok {
			return 0
		}
		return i
	}
	return 0
}

// ---------------------------------------------------------------------------
// Tool-call ids
// ---------------------------------------------------------------------------

const shortToolIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var fallbackIDCounter atomic.Uint64

// shortToolID mirrors _short_tool_id (openai_compat_provider.py:230-232):
// a 9-character alphanumeric id compatible with every provider, including
// Mistral, which rejects longer OpenAI ids.
func shortToolID() string {
	out := make([]byte, 9)
	buf := make([]byte, 1)
	for i := range out {
		for {
			if _, err := rand.Read(buf); err != nil {
				return fallbackToolID()
			}
			// Reject the top of the byte range so the 62-character alphabet is
			// sampled without modulo bias.
			if buf[0] < 248 {
				out[i] = shortToolIDAlphabet[int(buf[0])%len(shortToolIDAlphabet)]
				break
			}
		}
	}
	return string(out)
}

// fallbackToolID is used only if the system CSPRNG fails; ids must stay unique
// so replayed tool results cannot collide.
func fallbackToolID() string {
	seed := uint64(time.Now().UnixNano()) ^ fallbackIDCounter.Add(1)
	text := strconv.FormatUint(seed, 36)
	if len(text) < 9 {
		text = strings.Repeat("0", 9-len(text)) + text
	}
	return text[len(text)-9:]
}
