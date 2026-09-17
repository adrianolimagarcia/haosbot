// Package core defines the provider-independent vocabulary of nanobot-go.
//
// Everything in this package is deliberately free of any provider, channel or
// storage concern. Providers translate to/from these types; the agent loop,
// runner, session store and tools only ever speak these types.
//
// Compatibility note: this vocabulary mirrors the Python reference at
// upstream/nanobot @ 1bb712d3488915ca4ed9ccc1a93067ff722f5ab9, specifically
// nanobot/bus/events.py (InboundMessage/OutboundMessage) and
// nanobot/providers/base.py (LLMResponse/ToolCallRequest/LLMUsage).
package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/filediff"
)

// Role is a conversation participant role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Inbound / outbound bus messages
// ---------------------------------------------------------------------------

// InboundMessage is a message received from a chat channel.
// Mirrors nanobot/bus/events.py:InboundMessage.
type InboundMessage struct {
	Channel             string         `json:"channel"`
	SenderID            string         `json:"sender_id"`
	ChatID              string         `json:"chat_id"`
	Content             string         `json:"content"`
	Timestamp           time.Time      `json:"timestamp"`
	Media               []string       `json:"media"`
	Metadata            map[string]any `json:"metadata"`
	SessionKeyOverride  *string        `json:"session_key_override"`
	RequireExistingSess bool           `json:"require_existing_session"`
	InputRole           *string        `json:"input_role"`
}

// SessionKey is the unique key identifying the conversation session.
// Mirrors InboundMessage.session_key: override wins, else "<channel>:<chat_id>".
func (m *InboundMessage) SessionKey() string {
	if m.SessionKeyOverride != nil && *m.SessionKeyOverride != "" {
		return *m.SessionKeyOverride
	}
	return m.Channel + ":" + m.ChatID
}

// IsUserInput reports whether this message enters the conversation as user input.
// Mirrors InboundMessage.is_user_input.
func (m *InboundMessage) IsUserInput() bool {
	if m.InputRole != nil {
		return *m.InputRole == string(RoleUser)
	}
	return m.Channel != "system"
}

// AgentEvent is a typed, transport-independent notification carried alongside
// an outbound message.
//
// Upstream declares OutboundMessage.event with a TYPE_CHECKING-only import of
// nanobot.events.AgentEvent (bus/events.py:13-14) specifically so the bus does
// not depend on the event hierarchy. The interface therefore lives here, in the
// shared vocabulary package, and the concrete events live in internal/events —
// which imports this package, never the other way round.
type AgentEvent interface {
	// EventName identifies the event kind for diagnostics and filtering.
	EventName() string
}

// OutboundMessage is a message to send to a chat channel.
// Mirrors nanobot/bus/events.py:OutboundMessage.
//
// Event carries internal runtime/UI semantics; Metadata stays reserved for
// channel routing context. Keeping the two separate is deliberate upstream: an
// earlier design reserved a metadata key for this, which made "is this message
// a stream delta or a finished reply?" unanswerable without inspecting strings.
type OutboundMessage struct {
	Channel  string         `json:"channel"`
	ChatID   string         `json:"chat_id"`
	Content  string         `json:"content"`
	ReplyTo  *string        `json:"reply_to"`
	Media    []string       `json:"media"`
	Metadata map[string]any `json:"metadata"`
	Buttons  [][]string     `json:"buttons"`
	Event    AgentEvent     `json:"event,omitempty"`
}

// ---------------------------------------------------------------------------
// Conversation content
// ---------------------------------------------------------------------------

// Content is a message body that is either a plain string or a list of content
// blocks (multimodal). Python models this as “str | list[dict]“.
//
// Content preserves the distinction between a JSON string and a JSON array on
// the wire, which matters for round-trip fidelity with the Python runtime.
type Content struct {
	// Text is set when the content is a plain string.
	Text string
	// Blocks is set when the content is a list of content blocks.
	Blocks []ContentBlock
	// isText records which representation is authoritative. This matters
	// because an empty string and an empty block list are distinct on disk.
	isText bool
}

// TextContent returns a Content holding a plain string.
func TextContent(s string) Content { return Content{Text: s, isText: true} }

// BlockContent returns a Content holding content blocks.
func BlockContent(b []ContentBlock) Content { return Content{Blocks: b} }

// IsText reports whether the content is a plain string.
func (c Content) IsText() bool { return c.isText }

// IsZero reports whether the content carries nothing at all.
func (c Content) IsZero() bool { return !c.isText && c.Blocks == nil }

// MarshalJSON writes a JSON string or JSON array, matching Python's shape.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.isText {
		return json.Marshal(c.Text)
	}
	if c.Blocks == nil {
		return []byte("null"), nil
	}
	return json.Marshal(c.Blocks)
}

// UnmarshalJSON accepts either a JSON string or a JSON array of blocks.
func (c *Content) UnmarshalJSON(data []byte) error {
	trimmed := trimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("core: empty content")
	}
	switch trimmed[0] {
	case 'n': // null
		*c = Content{}
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*c = TextContent(s)
		return nil
	case '[':
		var blocks []ContentBlock
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return err
		}
		*c = BlockContent(blocks)
		return nil
	default:
		return fmt.Errorf("core: content must be a string or array, got %q", trimmed[0])
	}
}

// ContentBlock is one element of a multimodal content array.
//
// The Python runtime passes provider-shaped block dicts through largely
// opaquely, so this type keeps both the known fields and the raw form.
type ContentBlock struct {
	// Type is the block discriminator, e.g. "text", "image_url", "input_image".
	Type string `json:"type"`
	// Text is set for text blocks.
	Text string `json:"text,omitempty"`
	// ImageURL is set for image blocks (provider-shaped).
	ImageURL json.RawMessage `json:"image_url,omitempty"`
	// Raw holds the complete original object so unknown fields survive a
	// parse/serialize round trip.
	Raw map[string]json.RawMessage `json:"-"`
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if b.Raw == nil {
		type alias ContentBlock
		return json.Marshal(alias(b))
	}
	out := make(map[string]json.RawMessage, len(b.Raw)+2)
	for k, v := range b.Raw {
		out[k] = v
	}
	if b.Type != "" {
		if enc, err := json.Marshal(b.Type); err == nil {
			out["type"] = enc
		}
	}
	if b.Text != "" {
		if enc, err := json.Marshal(b.Text); err == nil {
			out["text"] = enc
		}
	}
	if len(b.ImageURL) > 0 {
		out["image_url"] = b.ImageURL
	}
	return json.Marshal(out)
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.Raw = raw
	if v, ok := raw["type"]; ok {
		_ = json.Unmarshal(v, &b.Type)
	}
	if v, ok := raw["text"]; ok {
		_ = json.Unmarshal(v, &b.Text)
	}
	if v, ok := raw["image_url"]; ok {
		b.ImageURL = v
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tool calls and results
// ---------------------------------------------------------------------------

// ToolCall is a tool invocation requested by the model.
// Mirrors nanobot/providers/base.py:ToolCallRequest.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments holds the raw JSON arguments exactly as received. It is kept
	// raw so a parse/serialize round trip is byte-faithful.
	Arguments json.RawMessage `json:"arguments"`
	// ExtraContent and ProviderSpecificFields are provider passthroughs.
	ExtraContent             map[string]any `json:"extra_content,omitempty"`
	ProviderSpecificFields   map[string]any `json:"provider_specific_fields,omitempty"`
	FunctionProviderSpecific map[string]any `json:"function_provider_specific_fields,omitempty"`
}

// HasValidName reports whether the call carries a usable (non-empty) name.
// Mirrors ToolCallRequest.has_valid_name: a degenerate call must never be
// persisted and replayed, because providers reject the whole request.
func (t *ToolCall) HasValidName() bool { return t.Name != "" }

// ArgumentsString returns the arguments as a JSON string.
// A nil argument set is reported as "{}", matching the Python no-arg case.
func (t *ToolCall) ArgumentsString() string {
	if len(t.Arguments) == 0 {
		return "{}"
	}
	return string(t.Arguments)
}

// ToolResult is the outcome of executing a tool.
type ToolResult struct {
	// CallID is the id of the ToolCall this result answers.
	CallID string
	// Content is the textual result injected back into the conversation.
	Content string
	// IsError marks a failed execution. Errors are represented as content,
	// not as a Go error, so the model can observe and recover from them.
	IsError bool
	// FileDiffs carries the line-level alignment of each file a mutating
	// filesystem tool changed, keyed by resolved absolute path.
	//
	// It mirrors the file_diffs attribute of the reference's FileEditResult
	// (utils/file_edit_events.py:135-147): the value a write-capable tool
	// returns so the file-edit tracker can build its activity event without
	// recomputing the diff. nil for every other tool and for failures.
	FileDiffs map[string]filediff.FileDiff
}

// ---------------------------------------------------------------------------
// Model responses
// ---------------------------------------------------------------------------

// FinishReason is why the model stopped generating.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishFunctionCall  FinishReason = "function_call"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
	FinishRefusal       FinishReason = "refusal"
	FinishError         FinishReason = "error"
)

// Response is a model response.
// Mirrors nanobot/providers/base.py:LLMResponse.
type Response struct {
	Content          string           `json:"content"`
	HasContent       bool             `json:"-"`
	ToolCalls        []ToolCall       `json:"tool_calls"`
	FinishReason     FinishReason     `json:"finish_reason"`
	Usage            *Usage           `json:"usage"`
	ReasoningContent string           `json:"reasoning_content"`
	ThinkingBlocks   []map[string]any `json:"thinking_blocks"`

	// Streaming telemetry (locally measured, not provider-reported).
	GenerationMS *int64 `json:"generation_ms"`
	TTFTMS       *int64 `json:"ttft_ms"`

	// RetryAfter is a provider-supplied retry wait in seconds.
	RetryAfter *float64 `json:"retry_after"`

	// Structured error metadata, used by the retry policy when
	// FinishReason == FinishError.
	ErrorStatusCode  *int     `json:"error_status_code"`
	ErrorKind        string   `json:"error_kind"`
	ErrorType        string   `json:"error_type"`
	ErrorCode        string   `json:"error_code"`
	ErrorRetryAfterS *float64 `json:"error_retry_after_s"`
	ErrorShouldRetry *bool    `json:"error_should_retry"`
}

// HasToolCalls reports whether the response contains tool calls.
func (r *Response) HasToolCalls() bool { return len(r.ToolCalls) > 0 }

// ShouldExecuteTools mirrors LLMResponse.should_execute_tools: tools execute
// only when tool calls are present AND the finish reason is tool-capable.
// This deliberately blocks gateway-injected calls under refusal/content_filter/error.
func (r *Response) ShouldExecuteTools() bool {
	if !r.HasToolCalls() {
		return false
	}
	switch r.FinishReason {
	case FinishToolCalls, FinishFunctionCall, FinishStop:
		return true
	}
	return false
}

// Usage is token accounting for one model call.
// Mirrors nanobot/providers/base.py:LLMUsage.
type Usage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	CachedTokens     *int `json:"cached_tokens,omitempty"`
	ReasoningTokens  *int `json:"reasoning_tokens,omitempty"`
}

// Add accumulates other into u.
func (u *Usage) Add(other *Usage) {
	if other == nil {
		return
	}
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	if other.CachedTokens != nil {
		if u.CachedTokens == nil {
			u.CachedTokens = new(int)
		}
		*u.CachedTokens += *other.CachedTokens
	}
	if other.ReasoningTokens != nil {
		if u.ReasoningTokens == nil {
			u.ReasoningTokens = new(int)
		}
		*u.ReasoningTokens += *other.ReasoningTokens
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// StreamEventKind discriminates a stream delta.
type StreamEventKind int

const (
	// StreamText is a chunk of assistant-visible text.
	StreamText StreamEventKind = iota
	// StreamReasoning is a chunk of reasoning/thinking text.
	StreamReasoning
	// StreamToolCall is an incremental tool-call fragment.
	StreamToolCall
	// StreamUsage reports token usage for the completed call.
	StreamUsage
	// StreamDone terminates the stream.
	StreamDone
)

// StreamEvent is one incremental event from a streaming model response.
//
// A StreamEvent is a union: only the fields relevant to Kind are meaningful.
type StreamEvent struct {
	Kind StreamEventKind

	// Text is set for StreamText and StreamReasoning.
	Text string

	// Tool-call fragment fields, set for StreamToolCall. Providers emit the
	// call id and name on the first fragment for an index and only
	// ArgumentsDelta afterwards, so Index identifies the target call.
	Index          int
	ToolCallID     string
	ToolCallName   string
	ArgumentsDelta string

	// Usage is set for StreamUsage.
	Usage *Usage

	// Response is set for StreamDone and carries the aggregated final result.
	Response *Response

	// Err is set when the stream terminated due to an error.
	Err error
}

// String renders a stream event for debugging.
func (e StreamEvent) String() string {
	switch e.Kind {
	case StreamText:
		return fmt.Sprintf("text(%d bytes)", len(e.Text))
	case StreamReasoning:
		return fmt.Sprintf("reasoning(%d bytes)", len(e.Text))
	case StreamToolCall:
		return fmt.Sprintf("tool_call(idx=%d name=%q)", e.Index, e.ToolCallName)
	case StreamUsage:
		return "usage"
	case StreamDone:
		return "done"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// UnmarshalJSON accepts both shapes a tool call appears in.
//
// Providers send the OpenAI nested form:
//
//	{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}
//
// while this package's own storage and wire format use the flat form:
//
//	{"id":"c1","name":"read_file","arguments":"{}"}
//
// Without this method the nested form decoded to a zero-valued ToolCall with
// no name and no arguments — silently, because encoding/json ignores unknown
// keys. The session layer compensated by re-converting after decoding, but any
// other caller of Message.UnmarshalJSON would have received a degenerate call.
//
// When both shapes are present the nested one wins, matching the reference's
// _normalize_tool_call, which reads the flat fields first and then overwrites
// them from "function".
func (t *ToolCall) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	*t = ToolCall{}

	if v, ok := fields["id"]; ok {
		_ = json.Unmarshal(v, &t.ID)
	}
	name, hasName := fields["name"]
	arguments, hasArguments := fields["arguments"]

	if fn, ok := fields["function"]; ok {
		var fnFields map[string]json.RawMessage
		if err := json.Unmarshal(fn, &fnFields); err == nil {
			if v, ok := fnFields["name"]; ok {
				name, hasName = v, true
			}
			if v, ok := fnFields["arguments"]; ok {
				arguments, hasArguments = v, true
			}
			if v, ok := fnFields["provider_specific_fields"]; ok {
				_ = json.Unmarshal(v, &t.FunctionProviderSpecific)
			}
		}
	}

	if hasName {
		_ = json.Unmarshal(name, &t.Name)
	}
	if hasArguments {
		// Kept exactly as received: this package's contract is a
		// byte-faithful round trip. Unwrapping a string-encoded argument
		// payload is a storage-format concern and belongs to the session
		// layer, which calls NormalizeToolArguments explicitly.
		//
		// An explicit null is folded into "absent" so the two spellings of
		// "no arguments" behave identically. Without this, a nil Arguments
		// marshals to null and decodes back as the 4-byte token, so the value
		// would differ from itself after one round trip.
		trimmed := bytes.TrimSpace(arguments)
		if !bytes.Equal(trimmed, []byte("null")) {
			t.Arguments = append(json.RawMessage(nil), trimmed...)
		}
	}
	if v, ok := fields["extra_content"]; ok {
		_ = json.Unmarshal(v, &t.ExtraContent)
	}
	if v, ok := fields["provider_specific_fields"]; ok {
		_ = json.Unmarshal(v, &t.ProviderSpecificFields)
	}
	return nil
}

// NormalizeToolArguments unwraps arguments that arrived as a JSON string.
//
// A provider may send arguments as an object:
//
//	{"path": "a.txt"}
//
// or as a string containing that object:
//
//	"{\"path\": \"a.txt\"}"
//
// Both mean the same thing, and callers must not have to know which arrived.
// A string that is not itself valid JSON is left as the original quoted token,
// because it is still the argument payload and dropping it would lose data.
func NormalizeToolArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '"' {
		return append(json.RawMessage(nil), trimmed...)
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return append(json.RawMessage(nil), trimmed...)
	}
	inner := bytes.TrimSpace([]byte(s))
	if len(inner) > 0 && json.Valid(inner) {
		return append(json.RawMessage(nil), inner...)
	}
	return append(json.RawMessage(nil), trimmed...)
}
