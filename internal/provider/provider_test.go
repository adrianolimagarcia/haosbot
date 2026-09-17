package provider

import (
	"reflect"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func TestChatRequestApplyDefaults(t *testing.T) {
	req := ChatRequest{}
	req.ApplyDefaults()

	if req.MaxTokens != DefaultMaxTokens {
		t.Errorf("ApplyDefaults MaxTokens = %d, want %d", req.MaxTokens, DefaultMaxTokens)
	}
	if req.Temperature != DefaultTemperature {
		t.Errorf("ApplyDefaults Temperature = %f, want %f", req.Temperature, DefaultTemperature)
	}

	// Preset values must not be overridden
	reqCustom := ChatRequest{MaxTokens: 100, Temperature: 0.2}
	reqCustom.ApplyDefaults()
	if reqCustom.MaxTokens != 100 || reqCustom.Temperature != 0.2 {
		t.Errorf("ApplyDefaults overwrote custom values: %+v", reqCustom)
	}
}

func TestHTTPError(t *testing.T) {
	err := &HTTPError{
		StatusCode: 429,
		Status:     "Too Many Requests",
		Body:       "rate limit hit",
		RetryAfter: 5.0,
	}

	if !err.Retryable() {
		t.Errorf("HTTPError(429).Retryable() = false, want true")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "Too Many Requests") {
		t.Errorf("HTTPError.Error() = %q", err.Error())
	}

	// Truncate long body
	longBody := strings.Repeat("x", 600)
	errLong := &HTTPError{StatusCode: 500, Status: "Internal Server Error", Body: longBody}
	if !strings.HasSuffix(errLong.Error(), "...") {
		t.Errorf("HTTPError long body was not truncated with '...'")
	}

	// Retryable status codes: 408, 409, 429, >=500
	retryableCodes := []int{408, 409, 429, 500, 502, 503, 504}
	for _, code := range retryableCodes {
		e := &HTTPError{StatusCode: code}
		if !e.Retryable() {
			t.Errorf("HTTPError(%d).Retryable() = false, want true", code)
		}
	}
	nonRetryableCodes := []int{400, 401, 403, 404, 422}
	for _, code := range nonRetryableCodes {
		e := &HTTPError{StatusCode: code}
		if e.Retryable() {
			t.Errorf("HTTPError(%d).Retryable() = true, want false", code)
		}
	}
}

func TestToolSchemaOpenAITool(t *testing.T) {
	// Empty parameters
	schemaEmpty := ToolSchema{
		Name:        "get_weather",
		Description: "Fetch weather",
	}
	tool := schemaEmpty.OpenAITool()
	fn, ok := tool["function"].(map[string]any)
	if !ok || fn["name"] != "get_weather" {
		t.Fatalf("OpenAITool() invalid function: %+v", tool)
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("OpenAITool() default parameters not object: %+v", fn)
	}

	// Complex parameters with array type (like ["string", "null"] for CloudCode sanitization)
	rawJSON := []byte(`{
		"type": ["object", "null"],
		"properties": {
			"location": {
				"type": ["string", "null"],
				"description": "City"
			}
		}
	}`)
	schemaComplex := ToolSchema{
		Name:        "get_weather",
		Description: "Fetch weather",
		Parameters:  rawJSON,
	}
	toolComplex := schemaComplex.OpenAITool()
	fnComplex := toolComplex["function"].(map[string]any)
	paramsComplex := fnComplex["parameters"].(map[string]any)

	// "type" must be sanitized to single string "object"
	if paramsComplex["type"] != "object" {
		t.Errorf("sanitizeSchema didn't flatten array type: got %v, want 'object'", paramsComplex["type"])
	}
	props := paramsComplex["properties"].(map[string]any)
	loc := props["location"].(map[string]any)
	if loc["type"] != "string" {
		t.Errorf("sanitizeSchema didn't flatten property array type: got %v, want 'string'", loc["type"])
	}
}

func TestClassifyResponse(t *testing.T) {
	// Nil response
	transient, delay := ClassifyResponse(nil)
	if transient || delay != 0 {
		t.Errorf("ClassifyResponse(nil) = %v, %v, want false, 0", transient, delay)
	}

	// ErrorShouldRetry explicit overrides
	retryTrue := true
	retryDelay := 12.5
	respOverride := &core.Response{
		ErrorShouldRetry: &retryTrue,
		ErrorRetryAfterS: &retryDelay,
	}
	transient, delay = ClassifyResponse(respOverride)
	if !transient || delay != 12.5 {
		t.Errorf("ClassifyResponse override = %v, %v, want true, 12.5", transient, delay)
	}

	// Non-error finish reason
	respNonError := &core.Response{FinishReason: core.FinishStop}
	transient, _ = ClassifyResponse(respNonError)
	if transient {
		t.Errorf("ClassifyResponse(FinishStop) = true, want false")
	}

	// Status 500 error
	code500 := 500
	resp500 := &core.Response{
		FinishReason:    core.FinishError,
		ErrorStatusCode: &code500,
	}
	transient, _ = ClassifyResponse(resp500)
	if !transient {
		t.Errorf("ClassifyResponse(500) = false, want true")
	}

	// Status 429 - rate limit (transient)
	code429 := 429
	resp429Rate := &core.Response{
		FinishReason:    core.FinishError,
		ErrorStatusCode: &code429,
		ErrorType:       "rate_limit_exceeded",
	}
	transient, _ = ClassifyResponse(resp429Rate)
	if !transient {
		t.Errorf("ClassifyResponse(429 rate limit) = false, want true")
	}

	// Status 429 - insufficient quota (non-transient)
	resp429Quota := &core.Response{
		FinishReason:    core.FinishError,
		ErrorStatusCode: &code429,
		ErrorType:       "insufficient_quota",
	}
	transient, _ = ClassifyResponse(resp429Quota)
	if transient {
		t.Errorf("ClassifyResponse(429 quota) = true, want false")
	}

	// Status 429 - text marker quota (non-transient)
	resp429QuotaText := &core.Response{
		FinishReason:    core.FinishError,
		ErrorStatusCode: &code429,
		Content:         "You exceeded your current quota, please check your plan",
	}
	transient, _ = ClassifyResponse(resp429QuotaText)
	if transient {
		t.Errorf("ClassifyResponse(429 quota text) = true, want false")
	}

	// ErrorKind timeout / connection
	respTimeout := &core.Response{
		FinishReason: core.FinishError,
		ErrorKind:    "timeout",
	}
	transient, _ = ClassifyResponse(respTimeout)
	if !transient {
		t.Errorf("ClassifyResponse(timeout) = false, want true")
	}

	// Content text marker
	respContentMarker := &core.Response{
		FinishReason: core.FinishError,
		Content:      "Service is temporarily unavailable",
	}
	transient, _ = ClassifyResponse(respContentMarker)
	if !transient {
		t.Errorf("ClassifyResponse(transient marker) = false, want true")
	}
}

func TestIsTransientErrorText(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"429 Too Many Requests", true},
		{"Connection timed out", true},
		{"Server error 503", true},
		{"速率限制", true},
		{"Invalid authorization key", false},
		{"Not found", false},
	}

	for _, tc := range cases {
		got := IsTransientErrorText(tc.in)
		if got != tc.want {
			t.Errorf("IsTransientErrorText(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	// Provider retryAfter takes precedence (+ RetryAfterBuffer)
	if got := RetryDelay(1, 10.0, nil); got != 11.0 {
		t.Errorf("RetryDelay with retryAfter = %f, want 11.0", got)
	}

	// Attempt fallback to schedule
	delays := []float64{1.0, 2.0, 4.0}
	if got := RetryDelay(1, 0, delays); got != 1.0 {
		t.Errorf("RetryDelay attempt 1 = %f, want 1.0", got)
	}
	if got := RetryDelay(2, 0, delays); got != 2.0 {
		t.Errorf("RetryDelay attempt 2 = %f, want 2.0", got)
	}
	if got := RetryDelay(3, 0, delays); got != 4.0 {
		t.Errorf("RetryDelay attempt 3 = %f, want 4.0", got)
	}
	// Clamped to last entry
	if got := RetryDelay(10, 0, delays); got != 4.0 {
		t.Errorf("RetryDelay attempt 10 = %f, want 4.0", got)
	}
	// Negative/zero attempt index clamped to 0
	if got := RetryDelay(0, 0, delays); got != 1.0 {
		t.Errorf("RetryDelay attempt 0 = %f, want 1.0", got)
	}
}

func TestSanitizeSchemaTypes(t *testing.T) {
	// Test sanitizeSchema slice and string slice handling
	input := map[string]any{
		"type": []string{"string", "null"},
		"nested": []any{
			map[string]any{"type": []any{"number", "null"}},
		},
		"pure_null": []string{"null"},
	}
	sanitized := sanitizeSchema(input).(map[string]any)

	if sanitized["type"] != "string" {
		t.Errorf("sanitizeSchema []string failed: got %v, want 'string'", sanitized["type"])
	}
	nestedSlice := sanitized["nested"].([]any)
	nestedMap := nestedSlice[0].(map[string]any)
	if nestedMap["type"] != "number" {
		t.Errorf("sanitizeSchema nested []any failed: got %v, want 'number'", nestedMap["type"])
	}
	// "pure_null" is not key "type", so it is retained as []any or unchanged
	if reflect.TypeOf(sanitized["pure_null"]).Kind() != reflect.Slice {
		t.Errorf("sanitizeSchema non-type slice should remain slice: got %v", sanitized["pure_null"])
	}
}
