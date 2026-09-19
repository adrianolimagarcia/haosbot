package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

func waitForCondition(t *testing.T, timeout time.Duration, description string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

type fakeSessionTranscript struct {
	mu           sync.Mutex
	key          string
	messages     []core.Message
	meta         map[string]any
	updatedAt    time.Time
	lastArchived int
	saves        int
	checkpoints  []string
}

func newFakeSessionTranscript(key string) *fakeSessionTranscript {
	return &fakeSessionTranscript{
		key:       key,
		meta:      make(map[string]any),
		updatedAt: time.Now(),
	}
}

func (f *fakeSessionTranscript) Key() string { return f.key }

func (f *fakeSessionTranscript) Messages() []core.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.Message, len(f.messages))
	copy(out, f.messages)
	return out
}

func (f *fakeSessionTranscript) AddMessage(m core.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, m)
	f.updatedAt = time.Now()
}

func (f *fakeSessionTranscript) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = nil
	f.meta = make(map[string]any)
}

func (f *fakeSessionTranscript) Save() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	return nil
}

func (f *fakeSessionTranscript) Metadata() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meta
}

func (f *fakeSessionTranscript) UpdatedAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updatedAt
}

func (f *fakeSessionTranscript) LastArchived() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastArchived
}

func (f *fakeSessionTranscript) CommitSummaryCheckpoint(summary string, insertAt *int, lastActive *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	boundary := len(f.messages)
	if insertAt != nil {
		boundary = *insertAt
	}

	marker := core.Message{
		Role:    core.RoleUser,
		Content: core.TextContent("Continue the active task from the working-memory checkpoint above."),
	}
	marker.SetExtra("_hidden_history", []byte("true"))

	// Insert marker at boundary
	if boundary >= len(f.messages) {
		f.messages = append(f.messages, marker)
	} else {
		f.messages = append(f.messages[:boundary+1], f.messages[boundary:]...)
		f.messages[boundary] = marker
	}

	activeStr := f.updatedAt.Format(time.RFC3339)
	if lastActive != nil {
		activeStr = lastActive.Format(time.RFC3339)
	}
	f.meta["_last_summary"] = map[string]any{
		"text":        summary,
		"last_active": activeStr,
	}
	f.lastArchived = boundary
	f.checkpoints = append(f.checkpoints, summary)
}

func (f *fakeSessionTranscript) GetHistory(maxMessages, maxTokens int, extendToUser, includeRuntimeContext bool) []core.Message {
	f.mu.Lock()
	defer f.mu.Unlock()

	start := f.lastArchived
	if start < 0 {
		start = 0
	}
	if start > len(f.messages) {
		start = len(f.messages)
	}

	out := make([]core.Message, 0, len(f.messages)-start)
	for i := start; i < len(f.messages); i++ {
		m := f.messages[i]
		if raw, ok := m.Extra("_command"); ok && pyTruthy(raw) {
			continue
		}
		out = append(out, m)
	}
	return out
}

type fakeSessionStore struct {
	mu          sync.Mutex
	transcripts map[string]*fakeSessionTranscript
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{
		transcripts: make(map[string]*fakeSessionTranscript),
	}
}

func (s *fakeSessionStore) Open(key string) (Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transcripts[key]
	if !ok {
		t = newFakeSessionTranscript(key)
		s.transcripts[key] = t
	}
	return t, nil
}

type summarizingProvider struct {
	mu           sync.Mutex
	requests     []provider.ChatRequest
	summaryReply string
	chatReply    string
}

func (p *summarizingProvider) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)

	// Check if this is a summarization request
	for _, m := range req.Messages {
		if strings.Contains(m.Content.Text, "conversational summarizer") {
			reply := p.summaryReply
			if reply == "" {
				reply = "Summary: User worked on optimizing the web interaction."
			}
			return &core.Response{
				Content:      reply,
				FinishReason: core.FinishStop,
			}, nil
		}
	}

	reply := p.chatReply
	if reply == "" {
		reply = "Hello from AI!"
	}
	return &core.Response{
		Content:      reply,
		FinishReason: core.FinishStop,
	}, nil
}

func (p *summarizingProvider) Model() string { return "test-model" }
func (p *summarizingProvider) Name() string  { return "test-provider" }

// streamingOnlySummarizer models the deployed provider: a non-streaming call
// with a large max_tokens never returns, while the streamed form answers
// promptly. It records whether the summarize call took the streaming path.
type streamingOnlySummarizer struct {
	mu              sync.Mutex
	chatCalls       int
	streamCalls     int
	turnStreamCalls int
	summary         string
	maxTokensSeen   int
}

func (p *streamingOnlySummarizer) Name() string { return "streaming-only" }

// Chat always fails, exactly like a non-streaming request that the provider
// never answers within the client's 120 s request timeout.
func (p *streamingOnlySummarizer) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.mu.Lock()
	p.chatCalls++
	p.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *streamingOnlySummarizer) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan core.StreamEvent, error) {
	isSummary := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content.Text, "conversational summarizer") {
			isSummary = true
		}
	}

	p.mu.Lock()
	if isSummary {
		p.streamCalls++
		p.maxTokensSeen = req.MaxTokens
	} else {
		p.turnStreamCalls++
	}
	summary := p.summary
	if summary == "" {
		summary = "streamed checkpoint summary"
	}
	p.mu.Unlock()

	reply := "turn reply"
	if isSummary {
		reply = summary
	}

	events := make(chan core.StreamEvent, 2)
	go func() {
		defer close(events)
		events <- core.StreamEvent{Kind: core.StreamText, Text: reply}
		events <- core.StreamEvent{Kind: core.StreamDone, Response: &core.Response{
			Content:      reply,
			FinishReason: core.FinishStop,
		}}
	}()
	return events, nil
}

func (p *streamingOnlySummarizer) counts() (int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatCalls, p.streamCalls, p.maxTokensSeen
}

// TestAutoSummarizeUsesStreamingWhenAvailable is the regression test for the
// deployed failure: the summarize call must stream, because a non-streaming
// call with a large max_tokens never returns on that provider and burns the
// whole turn deadline.
func TestAutoSummarizeUsesStreamingWhenAvailable(t *testing.T) {
	ctx := context.Background()
	store := newFakeSessionStore()
	prov := &streamingOnlySummarizer{summary: "streamed summary text"}

	b := bus.New(bus.Options{})
	defer b.Close()

	loop, err := NewLoop(LoopConfig{
		Bus:                  b,
		Store:                store,
		Provider:             prov,
		Tools:                tools.NewRegistry(),
		Prompt:               prompt.New(t.TempDir()),
		Model:                "test-model",
		ContextWindowTokens:  128_000,
		AutoSummarizeTokens:       100,
		AutoSummarizeTimeout:      5 * time.Second,
		BackgroundMaintenanceDelay: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	tr, _ := store.Open("webui:stream-test")
	sess := tr.(*fakeSessionTranscript)
	big := strings.Repeat("conversation filler text ", 200)
	sess.AddMessage(*core.NewMessage(core.RoleUser, big))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, big))

	if _, err := loop.ProcessMessage(ctx, core.InboundMessage{
		Channel: "webui", ChatID: "stream-test", Content: "next",
	}); err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}

	waitForCondition(t, time.Second, "background streamed summary", func() bool {
		_, streamCalls, _ := prov.counts()
		return streamCalls == 1
	})
	chatCalls, streamCalls, maxTokens := prov.counts()
	if chatCalls != 0 {
		t.Errorf("summarize used the non-streaming path %d time(s); it must stream", chatCalls)
	}
	if streamCalls != 1 {
		t.Fatalf("streamCalls=%d want 1", streamCalls)
	}
	if maxTokens != DefaultSummarizeMaxTokens {
		t.Errorf("summarize max_tokens=%d want %d", maxTokens, DefaultSummarizeMaxTokens)
	}
	if len(sess.checkpoints) != 1 || sess.checkpoints[0] != "streamed summary text" {
		t.Errorf("checkpoints=%v", sess.checkpoints)
	}
}

// TestAutoSummarizeFailureDoesNotRetryEveryTurn covers the wedging defect: once
// a summarize attempt fails, the session stays over the threshold on every
// following turn, so an unconditional retry would spend the turn deadline
// again and again.
func TestAutoSummarizeFailureDoesNotRetryEveryTurn(t *testing.T) {
	ctx := context.Background()
	store := newFakeSessionStore()
	prov := &failingSummarizer{}

	b := bus.New(bus.Options{})
	defer b.Close()

	loop, err := NewLoop(LoopConfig{
		Bus:                  b,
		Store:                store,
		Provider:             prov,
		Tools:                tools.NewRegistry(),
		Prompt:               prompt.New(t.TempDir()),
		Model:                "test-model",
		ContextWindowTokens:  128_000,
		AutoSummarizeTokens:       100,
		AutoSummarizeTimeout:      2 * time.Second,
		BackgroundMaintenanceDelay: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	tr, _ := store.Open("webui:wedge-test")
	sess := tr.(*fakeSessionTranscript)
	big := strings.Repeat("conversation filler text ", 200)
	sess.AddMessage(*core.NewMessage(core.RoleUser, big))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, big))

	if _, err := loop.ProcessMessage(ctx, core.InboundMessage{
		Channel: "webui", ChatID: "wedge-test", Content: "turn 0",
	}); err != nil {
		t.Fatalf("ProcessMessage 0: %v", err)
	}
	waitForCondition(t, time.Second, "first failed background summary", func() bool {
		return prov.summarizeCalls() == 1
	})

	for i := 1; i < 4; i++ {
		if _, err := loop.ProcessMessage(ctx, core.InboundMessage{
			Channel: "webui", ChatID: "wedge-test", Content: fmt.Sprintf("turn %d", i),
		}); err != nil {
			t.Fatalf("ProcessMessage %d: %v", i, err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	if got := prov.summarizeCalls(); got != 1 {
		t.Errorf("summarize attempted %d times across 4 turns; want 1 (hysteresis)", got)
	}
}

type failingSummarizer struct {
	mu    sync.Mutex
	calls int
}

func (p *failingSummarizer) Name() string { return "failing" }

func (p *failingSummarizer) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	for _, m := range req.Messages {
		if strings.Contains(m.Content.Text, "conversational summarizer") {
			p.mu.Lock()
			p.calls++
			p.mu.Unlock()
			return nil, errors.New("summarize unavailable")
		}
	}
	return &core.Response{Content: "ok", FinishReason: core.FinishStop}, nil
}

func (p *failingSummarizer) summarizeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestWebUIAutoSummarizeTrigger(t *testing.T) {
	ctx := context.Background()
	store := newFakeSessionStore()
	prov := &summarizingProvider{
		summaryReply: "Summary of past turns: discussed project goals.",
		chatReply:    "Understood, moving to next step.",
	}

	b := bus.New(bus.Options{})
	defer b.Close()

	ws := t.TempDir()
	promptBuilder := prompt.New(ws)

	// Configure with low threshold (e.g., 500 tokens) for easy triggering in test
	loop, err := NewLoop(LoopConfig{
		Bus:                 b,
		Store:               store,
		Provider:            prov,
		Tools:               tools.NewRegistry(),
		Prompt:              promptBuilder,
		Model:               "test-model",
		MaxTokens:           1024,
		ContextWindowTokens: 128_000,
		AutoSummarizeTokens:       500, // Trigger at 500 tokens for test
		BackgroundMaintenanceDelay: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	sessionKey := "webui:test-session"
	tr, err := store.Open(sessionKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess := tr.(*fakeSessionTranscript)

	// Seed the session with enough history to exceed 500 tokens
	largeText := strings.Repeat("This is a detailed conversation turn about software architecture and performance. ", 15)
	sess.AddMessage(*core.NewMessage(core.RoleUser, "Turn 1: "+largeText))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, "Turn 1 Reply: "+largeText))
	sess.AddMessage(*core.NewMessage(core.RoleUser, "Turn 2: "+largeText))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, "Turn 2 Reply: "+largeText))

	// Send message through WebUI
	msg := core.InboundMessage{
		Channel:  "webui",
		SenderID: "test-session",
		ChatID:   "test-session",
		Content:  "Turn 3: Let's continue.",
		Metadata: map[string]any{
			"source": "webui",
		},
	}

	out, err := loop.ProcessMessage(ctx, msg)
	if err != nil {
		t.Fatalf("ProcessMessage failed: %v", err)
	}
	if out.Content != "Understood, moving to next step." {
		t.Errorf("got content %q, want %q", out.Content, "Understood, moving to next step.")
	}

	// The triggering turn must NOT wait for summarization. Its provider request
	// happens first; the summary is generated only after the response and idle delay.
	prov.mu.Lock()
	initialReqs := append([]provider.ChatRequest(nil), prov.requests...)
	prov.mu.Unlock()
	if len(initialReqs) != 1 {
		t.Fatalf("triggering turn made %d provider requests before returning, want exactly 1", len(initialReqs))
	}
	for _, m := range initialReqs[0].Messages {
		if m.Role == core.RoleSystem && strings.Contains(m.Content.Text, "[Archived Context Summary]") {
			t.Fatal("triggering turn unexpectedly waited for the background summary")
		}
	}

	waitForCondition(t, time.Second, "background summary checkpoint", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return len(sess.checkpoints) == 1
	})

	sess.mu.Lock()
	if sess.checkpoints[0] != "Summary of past turns: discussed project goals." {
		t.Errorf("checkpoint summary = %q", sess.checkpoints[0])
	}
	lastSummary, ok := sess.meta["_last_summary"].(map[string]any)
	sess.mu.Unlock()
	if !ok {
		t.Fatalf("missing _last_summary in metadata")
	}
	if lastSummary["text"] != "Summary of past turns: discussed project goals." {
		t.Errorf("metadata summary = %v", lastSummary["text"])
	}

	// The NEXT turn consumes the ready checkpoint summary.
	next, err := loop.ProcessMessage(ctx, core.InboundMessage{
		Channel: "webui", SenderID: "test-session", ChatID: "test-session",
		Content: "Turn 4: use the compacted context.",
		Metadata: map[string]any{"source": "webui"},
	})
	if err != nil {
		t.Fatalf("next ProcessMessage failed: %v", err)
	}
	if next.Content != "Understood, moving to next step." {
		t.Errorf("next content = %q", next.Content)
	}

	prov.mu.Lock()
	reqs := append([]provider.ChatRequest(nil), prov.requests...)
	prov.mu.Unlock()
	lastReq := reqs[len(reqs)-1]
	foundSummaryInSystemPrompt := false
	for _, m := range lastReq.Messages {
		if m.Role == core.RoleSystem && strings.Contains(m.Content.Text, "[Archived Context Summary]") {
			foundSummaryInSystemPrompt = true
			if !strings.Contains(m.Content.Text, "Summary of past turns: discussed project goals.") {
				t.Errorf("system prompt did not include the summary text: %s", m.Content.Text)
			}
		}
	}
	if !foundSummaryInSystemPrompt {
		t.Errorf("next turn system prompt did not contain [Archived Context Summary]")
	}

}

func TestManualCompactCommand(t *testing.T) {
	ctx := context.Background()
	store := newFakeSessionStore()
	prov := &summarizingProvider{
		summaryReply: "Manual compaction summary.",
	}

	b := bus.New(bus.Options{})
	defer b.Close()

	loop, err := NewLoop(LoopConfig{
		Bus:                 b,
		Store:               store,
		Provider:            prov,
		Tools:               tools.NewRegistry(),
		Prompt:              prompt.New(t.TempDir()),
		Model:               "test-model",
		ContextWindowTokens: 128_000,
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	sessionKey := "webui:compact-test"
	tr, _ := store.Open(sessionKey)
	sess := tr.(*fakeSessionTranscript)
	sess.AddMessage(*core.NewMessage(core.RoleUser, "Previous question"))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, "Previous answer"))

	out, err := loop.ProcessMessage(ctx, core.InboundMessage{
		Channel:  "webui",
		SenderID: "compact-test",
		ChatID:   "compact-test",
		Content:  "/compact",
	})
	if err != nil {
		t.Fatalf("ProcessMessage /compact: %v", err)
	}
	if !strings.Contains(out.Content, "Context compacted successfully.") {
		t.Errorf("unexpected reply for /compact: %q", out.Content)
	}
	if len(sess.checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(sess.checkpoints))
	}
	if sess.checkpoints[0] != "Manual compaction summary." {
		t.Errorf("got summary %q", sess.checkpoints[0])
	}
}

func TestWebUIBelowThresholdNoSummarize(t *testing.T) {
	ctx := context.Background()
	store := newFakeSessionStore()
	prov := &summarizingProvider{
		chatReply: "Standard reply.",
	}

	b := bus.New(bus.Options{})
	defer b.Close()

	loop, err := NewLoop(LoopConfig{
		Bus:                 b,
		Store:               store,
		Provider:            prov,
		Tools:               tools.NewRegistry(),
		Prompt:              prompt.New(t.TempDir()),
		Model:               "test-model",
		ContextWindowTokens: 128_000,
		AutoSummarizeTokens: 120_000, // standard 120k threshold
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	sessionKey := "webui:small-session"
	tr, _ := store.Open(sessionKey)
	sess := tr.(*fakeSessionTranscript)
	sess.AddMessage(*core.NewMessage(core.RoleUser, "Short message"))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, "Short answer"))

	out, err := loop.ProcessMessage(ctx, core.InboundMessage{
		Channel:  "webui",
		SenderID: "small-session",
		ChatID:   "small-session",
		Content:  "Another short question",
	})
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if out.Content != "Standard reply." {
		t.Errorf("got %q, want %q", out.Content, "Standard reply.")
	}

	// Should not have triggered auto-summarize
	if len(sess.checkpoints) != 0 {
		t.Errorf("expected 0 checkpoints, got %d", len(sess.checkpoints))
	}
}

func TestNonWebChannelNoAutoSummarizeByDefault(t *testing.T) {
	ctx := context.Background()
	store := newFakeSessionStore()
	prov := &summarizingProvider{
		chatReply: "CLI reply.",
	}

	b := bus.New(bus.Options{})
	defer b.Close()

	loop, err := NewLoop(LoopConfig{
		Bus:                 b,
		Store:               store,
		Provider:            prov,
		Tools:               tools.NewRegistry(),
		Prompt:              prompt.New(t.TempDir()),
		Model:               "test-model",
		ContextWindowTokens: 128_000,
		// AutoSummarizeTokens unset (0), so it only applies to webui
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	sessionKey := "cli:terminal-session"
	tr, _ := store.Open(sessionKey)
	sess := tr.(*fakeSessionTranscript)

	largeText := strings.Repeat("Lots of terminal text to make it large. ", 50)
	sess.AddMessage(*core.NewMessage(core.RoleUser, largeText))
	sess.AddMessage(*core.NewMessage(core.RoleAssistant, largeText))

	out, err := loop.ProcessMessage(ctx, core.InboundMessage{
		Channel:  "cli",
		SenderID: "user",
		ChatID:   "terminal-session",
		Content:  "Continue in CLI",
	})
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if out.Content != "CLI reply." {
		t.Errorf("got %q, want %q", out.Content, "CLI reply.")
	}
	if len(sess.checkpoints) != 0 {
		t.Errorf("expected 0 checkpoints for cli channel by default, got %d", len(sess.checkpoints))
	}
}
