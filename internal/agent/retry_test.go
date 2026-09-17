package agent

import (
	"context"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// countingProvider returns a scripted sequence of responses and records how
// many times it was called.
type countingProvider struct {
	responses []*core.Response
	calls     int
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	resp := p.responses[p.calls%len(p.responses)]
	p.calls++
	return resp, nil
}

func errResponse(status int, kind, errType, content string) *core.Response {
	return &core.Response{
		FinishReason:    core.FinishError,
		Content:         content,
		ErrorStatusCode: &status,
		ErrorKind:       kind,
		ErrorType:       errType,
	}
}

// TestRetryGivesUpAfterFourAttempts pins the attempt count.
//
// The reference schedule is _CHAT_RETRY_DELAYS = (1, 2, 4) with the loop
// breaking when `attempt > len(delays)` (base.py:1876), so a permanently
// failing transient error is attempted exactly FOUR times.
func TestRetryGivesUpAfterFourAttempts(t *testing.T) {
	p := &countingProvider{responses: []*core.Response{
		errResponse(503, "", "", "server error"),
	}}
	runner := NewRunner()

	res, err := runner.Run(context.Background(), RunSpec{
		Messages:    []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider:    p,
		RetryDelays: []float64{0, 0, 0}, // no real sleeping in tests
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.calls != 4 {
		t.Errorf("provider called %d times, want 4 (3 delays + final attempt)", p.calls)
	}
	if res.Error == "" {
		t.Error("expected the run to report an error after exhausting retries")
	}
}

// TestRetryRecoversOnTransientError verifies a transient failure that clears is
// not reported to the user.
func TestRetryRecoversOnTransientError(t *testing.T) {
	p := &countingProvider{responses: []*core.Response{
		errResponse(503, "", "", "server error"),
		errResponse(429, "", "rate_limit_exceeded", "rate limit"),
		{Content: "recovered", FinishReason: core.FinishStop},
	}}
	runner := NewRunner()

	res, err := runner.Run(context.Background(), RunSpec{
		Messages:    []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider:    p,
		RetryDelays: []float64{0, 0, 0},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.calls != 3 {
		t.Errorf("provider called %d times, want 3", p.calls)
	}
	if res.FinalContent != "recovered" {
		t.Errorf("final content = %q, want %q", res.FinalContent, "recovered")
	}
}

// TestRetrySkipsNonTransientError is the important negative case: a permanent
// failure must fail fast instead of being retried three more times.
func TestRetrySkipsNonTransientError(t *testing.T) {
	p := &countingProvider{responses: []*core.Response{
		errResponse(400, "", "", "invalid request"),
	}}
	runner := NewRunner()

	if _, err := runner.Run(context.Background(), RunSpec{
		Messages:    []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider:    p,
		RetryDelays: []float64{0, 0, 0},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("provider called %d times, want 1 (400 is not retryable)", p.calls)
	}
}

// TestRetryHonoursQuotaExhaustion429 pins the 429 sub-classification.
//
// A 429 is not uniformly retryable: quota exhaustion is permanent. Retrying it
// would hammer a dead account for 7 seconds before failing anyway.
func TestRetryHonoursQuotaExhaustion429(t *testing.T) {
	p := &countingProvider{responses: []*core.Response{
		errResponse(429, "", "insufficient_quota", "insufficient_quota"),
	}}
	runner := NewRunner()

	if _, err := runner.Run(context.Background(), RunSpec{
		Messages:    []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider:    p,
		RetryDelays: []float64{0, 0, 0},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("provider called %d times, want 1 (quota exhaustion is permanent)", p.calls)
	}
}

// TestRetryHonoursPaymentRequired pins the 402 case: arrearage is permanent.
func TestRetryHonoursPaymentRequired(t *testing.T) {
	p := &countingProvider{responses: []*core.Response{
		errResponse(402, "", "", "payment required"),
	}}
	runner := NewRunner()

	if _, err := runner.Run(context.Background(), RunSpec{
		Messages:    []core.Message{*core.NewMessage(core.RoleUser, "hi")},
		Provider:    p,
		RetryDelays: []float64{0, 0, 0},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("provider called %d times, want 1 (402 is not retryable)", p.calls)
	}
}

// TestRetryDelaySchedule pins the backoff schedule itself, including the
// Retry-After override and its one-second buffer.
func TestRetryDelaySchedule(t *testing.T) {
	delays := []float64{1, 2, 4}

	cases := []struct {
		name       string
		attempt    int
		retryAfter float64
		want       float64
	}{
		{"first attempt uses delays[0]", 1, 0, 1},
		{"second attempt uses delays[1]", 2, 0, 2},
		{"third attempt uses delays[2]", 3, 0, 4},
		{"beyond schedule clamps to last", 4, 0, 4},
		{"retry-after replaces schedule", 1, 7, 8},    // 7 + RETRY_AFTER_BUFFER
		{"retry-after honoured later too", 3, 10, 11}, // not clamped to 4
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := provider.RetryDelay(tc.attempt, tc.retryAfter, delays); got != tc.want {
				t.Errorf("RetryDelay(%d, %v) = %v, want %v", tc.attempt, tc.retryAfter, got, tc.want)
			}
		})
	}
}

// TestClassifyResponseTransientness pins the classification table against the
// reference's is_transient_response (base.py:1012).
func TestClassifyResponseTransientness(t *testing.T) {
	code := func(c int) *int { return &c }
	boolp := func(b bool) *bool { return &b }

	cases := []struct {
		name string
		resp *core.Response
		want bool
	}{
		{"nil", nil, false},
		{"success is not transient", &core.Response{FinishReason: core.FinishStop}, false},
		{"500 retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(500)}, true},
		{"503 retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(503)}, true},
		{"408 retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(408)}, true},
		{"409 retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(409)}, true},
		{"400 not retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(400)}, false},
		{"401 not retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(401)}, false},
		{"429 rate limit retryable", &core.Response{
			FinishReason: core.FinishError, ErrorStatusCode: code(429),
			ErrorType: "rate_limit_exceeded"}, true},
		{"429 quota not retryable", &core.Response{
			FinishReason: core.FinishError, ErrorStatusCode: code(429),
			ErrorType: "insufficient_quota"}, false},
		{"402 not retryable", &core.Response{FinishReason: core.FinishError, ErrorStatusCode: code(402)}, false},
		{"timeout kind retryable", &core.Response{
			FinishReason: core.FinishError, ErrorKind: "timeout"}, true},
		{"connection kind retryable", &core.Response{
			FinishReason: core.FinishError, ErrorKind: "connection"}, true},
		{"text marker fallback", &core.Response{
			FinishReason: core.FinishError, Content: "upstream returned 503 server error"}, true},
		{"explicit verdict wins", &core.Response{
			FinishReason: core.FinishError, ErrorStatusCode: code(400),
			ErrorShouldRetry: boolp(true)}, true},
		{"explicit false wins", &core.Response{
			FinishReason: core.FinishError, ErrorStatusCode: code(503),
			ErrorShouldRetry: boolp(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := provider.ClassifyResponse(tc.resp)
			if got != tc.want {
				t.Errorf("ClassifyResponse transient = %v, want %v", got, tc.want)
			}
		})
	}
}
