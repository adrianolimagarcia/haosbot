// Package provider defines the model-provider boundary.
//
// A Provider turns a provider-agnostic request into a core.Response. Nothing
// above this package knows which vendor is in use; nothing below it knows
// about sessions, tools or channels.
//
// The contract mirrors nanobot/providers/base.py:LLMProvider.chat
// (upstream @ 1bb712d3488915ca4ed9ccc1a93067ff722f5ab9, base.py:923).
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// ToolSchema is a tool definition advertised to the model.
//
// Parameters is a raw JSON Schema object, kept raw so a tool can supply an
// arbitrary schema without an intermediate representation losing fidelity.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// OpenAITool renders the schema in OpenAI function-tool form:
//
//	{"type":"function","function":{"name":...,"description":...,"parameters":{...}}}
//
// This is the shape nanobot sends for OpenAI-compatible providers
// (mirrors ToolCallRequest.to_openai_tool_call, base.py:86).
// sanitizeSchema recursively transforms JSON schema to ensure compatibility with
// strict proto-based endpoints (such as Google CloudCode / Gemini OpenAI proxies),
// where "type" cannot be an array like ["string", "null"].
func sanitizeSchema(val any) any {
	switch v := val.(type) {
	case map[string]any:
		res := make(map[string]any, len(v))
		for k, item := range v {
			if k == "type" {
				if arr, ok := item.([]any); ok {
					for _, t := range arr {
						if str, isStr := t.(string); isStr && str != "null" {
							res[k] = str
							break
						}
					}
					if _, found := res[k]; !found && len(arr) > 0 {
						res[k] = arr[0]
					}
					continue
				}
				if arr, ok := item.([]string); ok {
					for _, str := range arr {
						if str != "null" {
							res[k] = str
							break
						}
					}
					if _, found := res[k]; !found && len(arr) > 0 {
						res[k] = arr[0]
					}
					continue
				}
			}
			res[k] = sanitizeSchema(item)
		}
		return res
	case []any:
		res := make([]any, len(v))
		for i, item := range v {
			res[i] = sanitizeSchema(item)
		}
		return res
	default:
		return val
	}
}

func (s ToolSchema) OpenAITool() map[string]any {
	var parsed any
	if len(s.Parameters) > 0 {
		_ = json.Unmarshal(s.Parameters, &parsed)
	}
	if parsed == nil {
		parsed = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	sanitized := sanitizeSchema(parsed)

	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        s.Name,
			"description": s.Description,
			"parameters":  sanitized,
		},
	}
}

// ChatRequest is one model call.
//
// Mirrors the parameter list of LLMProvider.chat (base.py:923).
type ChatRequest struct {
	Messages []core.Message
	Tools    []ToolSchema

	Model           string
	MaxTokens       int
	Temperature     float64
	ReasoningEffort string
	// ToolChoice is "auto", "required", "none", or a specific-tool object.
	ToolChoice any
}

// Default generation settings, mirroring GenerationSettings (base.py:612).
const (
	DefaultTemperature = 0.7
	DefaultMaxTokens   = 4096
)

// ApplyDefaults fills unset generation settings with the reference defaults.
func (r *ChatRequest) ApplyDefaults() {
	if r.MaxTokens == 0 {
		r.MaxTokens = DefaultMaxTokens
	}
	if r.Temperature == 0 {
		r.Temperature = DefaultTemperature
	}
}

// Provider is a chat-completion backend.
type Provider interface {
	// Chat performs one completion and returns the aggregated response.
	//
	// A transport or protocol failure is returned as an error. A model-level
	// failure (rate limit, quota, refusal) is returned as a *core.Response
	// with FinishReason == FinishError, matching the Python contract where
	// errors surface as responses so the retry policy can classify them.
	Chat(ctx context.Context, req ChatRequest) (*core.Response, error)

	// Name identifies the provider for logging and diagnostics.
	Name() string
}

// StreamingProvider is implemented by providers that can stream deltas.
//
// The returned channel is closed by the provider when the stream ends. The
// final StreamDone event carries the aggregated response. Consumers must keep
// reading until close even after cancelling ctx, or the provider goroutine
// leaks.
type StreamingProvider interface {
	Provider
	ChatStream(ctx context.Context, req ChatRequest) (<-chan core.StreamEvent, error)
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	// ErrNoProvider indicates no provider is configured or resolvable.
	ErrNoProvider = errors.New("provider: none configured")
	// ErrUnknownProvider indicates a named provider is not registered.
	ErrUnknownProvider = errors.New("provider: unknown")
	// ErrCanceled indicates the request was cancelled by the caller.
	ErrCanceled = errors.New("provider: canceled")
)

// HTTPError is a non-2xx response from a provider endpoint.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter float64
}

func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > 512 {
		body = body[:512] + "..."
	}
	return fmt.Sprintf("provider: http %d %s: %s", e.StatusCode, e.Status, body)
}

// Retryable reports whether the status code is worth retrying.
// Mirrors _RETRYABLE_STATUS_CODES = {408, 409, 429} plus status >= 500
// (base.py:1021).
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == 408 || e.StatusCode == 409 ||
		e.StatusCode == 429 || e.StatusCode >= 500
}

// Retry policy constants, verbatim from the reference.
const (
	// RetryAfterBuffer is RETRY_AFTER_BUFFER (base.py:31): a provider-supplied
	// Retry-After is padded by one second before sleeping.
	RetryAfterBuffer = 1.0

	// ChatRetryDelays is _CHAT_RETRY_DELAYS (base.py:626). Three delays means
	// FOUR attempts in total: attempts 1, 2 and 3 sleep 1s, 2s and 4s, and
	// attempt 4 gives up.
	PersistentMaxDelay          = 60.0
	PersistentIdenticalErrorCap = 10
)

// ChatRetryDelays mirrors _CHAT_RETRY_DELAYS.
var ChatRetryDelays = []float64{1, 2, 4}

// transientErrorMarkers mirrors _TRANSIENT_ERROR_MARKERS (base.py:630).
var transientErrorMarkers = []string{
	"429", "rate limit", "500", "502", "503", "504", "overloaded",
	"timeout", "timed out", "connection", "server error", "server_error",
	"temporarily unavailable", "速率限制", "访问量过大",
}

// nonRetryable429Tokens mirrors _NON_RETRYABLE_429_ERROR_TOKENS (base.py:649).
var nonRetryable429Tokens = map[string]bool{
	"insufficient_quota": true, "quota_exceeded": true, "quota_exhausted": true,
	"billing_hard_limit_reached": true, "insufficient_balance": true,
	"credit_balance_too_low": true, "billing_not_active": true,
	"payment_required": true,
}

// retryable429Tokens mirrors _RETRYABLE_429_ERROR_TOKENS (base.py:659).
var retryable429Tokens = map[string]bool{
	"rate_limit_exceeded": true, "rate_limit_error": true,
	"too_many_requests": true, "request_limit_exceeded": true,
	"requests_limit_exceeded": true, "overloaded_error": true,
}

// nonRetryable429Markers mirrors _NON_RETRYABLE_429_TEXT_MARKERS (base.py:667).
var nonRetryable429Markers = []string{
	"insufficient_quota", "insufficient quota", "quota exceeded",
	"quota exhausted", "billing hard limit", "billing_hard_limit_reached",
	"billing not active", "insufficient balance", "insufficient_balance",
	"credit balance too low", "payment required", "out of credits",
	"out of quota", "exceeded your current quota",
}

// retryable429Markers mirrors _RETRYABLE_429_TEXT_MARKERS (base.py:683).
var retryable429Markers = []string{
	"rate limit", "rate_limit", "too many requests", "retry after",
	"try again in", "temporarily unavailable", "overloaded",
	"concurrency limit", "速率限制",
}

// isRetryable429 mirrors _is_retryable_429_response (base.py:1088).
//
// A 429 is NOT uniform: quota exhaustion and billing failures are permanent
// and must not be retried, while rate limiting is transient. Treating them
// alike would either hammer a dead account or give up on a recoverable limit.
func isRetryable429(r *core.Response) bool {
	if r.ErrorStatusCode != nil && *r.ErrorStatusCode == 402 {
		return false
	}
	tokens := []string{strings.ToLower(r.ErrorType), strings.ToLower(r.ErrorCode)}
	for _, tok := range tokens {
		if tok != "" && nonRetryable429Tokens[tok] {
			return false
		}
	}
	content := strings.ToLower(r.Content)
	if containsAny(content, nonRetryable429Markers) {
		return false
	}
	for _, tok := range tokens {
		if tok != "" && retryable429Tokens[tok] {
			return true
		}
	}
	if containsAny(content, retryable429Markers) {
		return true
	}
	// Unknown 429 defaults to WAIT + retry (base.py:1103).
	return true
}

// ClassifyResponse reports whether a response represents a transient failure
// that the runner should retry, and the provider-suggested delay.
//
// Mirrors LLMProvider.is_transient_response (base.py:1012) and
// _is_retryable_429_response (base.py:1088). Structured error metadata is
// preferred; text markers are the legacy fallback.
func ClassifyResponse(r *core.Response) (transient bool, retryAfter float64) {
	if r == nil {
		return false, 0
	}
	// An explicit provider verdict always wins.
	if r.ErrorShouldRetry != nil {
		return *r.ErrorShouldRetry, derefFloat(r.ErrorRetryAfterS)
	}
	if r.FinishReason != core.FinishError {
		return false, 0
	}
	if r.ErrorStatusCode != nil {
		code := *r.ErrorStatusCode
		if code == 429 {
			return isRetryable429(r), derefFloat(r.ErrorRetryAfterS)
		}
		if code == 408 || code == 409 || code >= 500 {
			return true, derefFloat(r.ErrorRetryAfterS)
		}
	}
	if r.ErrorKind == "timeout" || r.ErrorKind == "connection" {
		return true, derefFloat(r.ErrorRetryAfterS)
	}
	// Legacy fallback: match known transient markers in the error text.
	if containsAny(strings.ToLower(r.Content), transientErrorMarkers) {
		return true, derefFloat(r.ErrorRetryAfterS)
	}
	return false, 0
}

// IsTransientErrorText reports whether an error message contains a marker the
// reference treats as transient (base.py:1007). It exists so the runner and
// the classifier share one marker list instead of keeping two copies that can
// drift apart.
func IsTransientErrorText(text string) bool {
	return containsAny(strings.ToLower(text), transientErrorMarkers)
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// RetryDelay returns the delay before the next attempt.
//
// Mirrors base.py:1894-1896: a provider-supplied Retry-After replaces the
// schedule and is padded by RetryAfterBuffer; otherwise the fixed schedule is
// used, clamped to its last entry once attempts run past it.
func RetryDelay(attempt int, retryAfter float64, delays []float64) float64 {
	if len(delays) == 0 {
		delays = ChatRetryDelays
	}
	if retryAfter > 0 {
		return retryAfter + RetryAfterBuffer
	}
	idx := attempt - 1
	if idx >= len(delays) {
		idx = len(delays) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return delays[idx]
}

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
