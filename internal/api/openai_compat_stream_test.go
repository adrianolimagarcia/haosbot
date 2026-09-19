package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

type streamTestProvider struct {
	events <-chan core.StreamEvent
}

func (p streamTestProvider) Name() string { return "stream-test" }

func (p streamTestProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	return nil, nil
}

func (p streamTestProvider) ChatStream(context.Context, provider.ChatRequest) (<-chan core.StreamEvent, error) {
	return p.events, nil
}

func TestChatCompletionStreamHonorsRequestCancellation(t *testing.T) {
	// A buggy third-party provider may forget to close its channel after the
	// request is cancelled. The HTTP boundary must still return promptly.
	events := make(chan core.StreamEvent)
	s := &Server{provider: streamTestProvider{events: events}}
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.handleChatCompletionStream(recorder, ctx, provider.ChatRequest{}, "test-model")

	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"provider_stream_error"`) {
		t.Fatalf("cancelled stream did not report an SSE error: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("cancelled stream did not terminate the SSE response: %q", body)
	}
}

func TestChatCompletionStreamRejectsCloseWithoutDone(t *testing.T) {
	events := make(chan core.StreamEvent)
	close(events)
	s := &Server{provider: streamTestProvider{events: events}}
	recorder := httptest.NewRecorder()

	s.handleChatCompletionStream(recorder, context.Background(), provider.ChatRequest{}, "test-model")

	body := recorder.Body.String()
	if !strings.Contains(body, "Provider stream ended before completion") {
		t.Fatalf("truncated stream was reported as successful: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("truncated stream did not terminate the SSE response: %q", body)
	}
}

func TestChatCompletionStreamStopsAtDone(t *testing.T) {
	events := make(chan core.StreamEvent, 2)
	events <- core.StreamEvent{Kind: core.StreamDone, Response: &core.Response{FinishReason: core.FinishStop}}
	events <- core.StreamEvent{Kind: core.StreamText, Text: "must-not-be-emitted"}
	close(events)
	s := &Server{provider: streamTestProvider{events: events}}
	recorder := httptest.NewRecorder()

	s.handleChatCompletionStream(recorder, context.Background(), provider.ChatRequest{}, "test-model")

	body := recorder.Body.String()
	if strings.Contains(body, "must-not-be-emitted") {
		t.Fatalf("events after StreamDone leaked into response: %q", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("stream emitted an invalid number of terminal markers: %q", body)
	}
}
