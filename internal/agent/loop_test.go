package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ---------------------------------------------------------------------------
// Fake transcript store
// ---------------------------------------------------------------------------

type fakeTranscript struct {
	key      string
	messages []core.Message
	saves    int
	cleared  bool
	saveErr  error
}

func (f *fakeTranscript) Key() string               { return f.key }
func (f *fakeTranscript) Messages() []core.Message  { return f.messages }
func (f *fakeTranscript) AddMessage(m core.Message) { f.messages = append(f.messages, m) }
func (f *fakeTranscript) Clear()                    { f.messages = nil; f.cleared = true }
func (f *fakeTranscript) Save() error {
	f.saves++
	return f.saveErr
}

type fakeStore struct {
	mu          sync.Mutex
	transcripts map[string]*fakeTranscript
	openErr     error
}

func newFakeStore() *fakeStore {
	return &fakeStore{transcripts: map[string]*fakeTranscript{}}
}

func (s *fakeStore) Open(key string) (Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openErr != nil {
		return nil, s.openErr
	}
	t, ok := s.transcripts[key]
	if !ok {
		t = &fakeTranscript{key: key}
		s.transcripts[key] = t
	}
	return t, nil
}

func (s *fakeStore) get(key string) *fakeTranscript {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transcripts[key]
}

// newTestLoop builds a loop wired to fakes. The provider is left to the caller
// (set l.cfg.Provider) so each test can install its own scripted provider.
func newTestLoop(t *testing.T, store TranscriptStore, reg *tools.Registry) *Loop {
	t.Helper()
	b := bus.New(bus.Options{})
	t.Cleanup(b.Close)
	l, err := NewLoop(LoopConfig{
		Bus:          b,
		Store:        store,
		Provider:     &scriptedProvider{},
		Tools:        reg,
		Workspace:    t.TempDir(),
		SystemPrompt: "SYSTEM PROMPT",
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestLoopFullTurn(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "hello back", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p

	out, err := l.ProcessMessage(context.Background(), core.InboundMessage{
		Channel: "cli", ChatID: "1", Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("nil outbound")
	}
	if out.Content != "hello back" {
		t.Errorf("content = %q", out.Content)
	}
	if out.Channel != "cli" || out.ChatID != "1" {
		t.Errorf("routing lost: %+v", out)
	}

	tr := store.get("cli:1")
	if tr == nil {
		t.Fatal("session not created")
	}
	// Transcript: user, assistant.
	if len(tr.messages) != 2 {
		t.Fatalf("transcript = %d messages, want 2: %+v", len(tr.messages), tr.messages)
	}
	if tr.messages[0].Role != core.RoleUser || tr.messages[0].Content.Text != "hello" {
		t.Errorf("user message wrong: %+v", tr.messages[0])
	}
	if tr.messages[1].Role != core.RoleAssistant || tr.messages[1].Content.Text != "hello back" {
		t.Errorf("assistant message wrong: %+v", tr.messages[1])
	}
	// The system prompt must NOT be persisted into the session.
	for _, m := range tr.messages {
		if m.Role == core.RoleSystem {
			t.Error("system prompt was persisted into the session")
		}
	}
}

func TestLoopSendsSystemPromptAndHistory(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "first", FinishReason: core.FinishStop},
		{Content: "second", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p
	ctx := context.Background()

	if _, err := l.ProcessMessage(ctx, core.InboundMessage{Channel: "cli", ChatID: "1", Content: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ProcessMessage(ctx, core.InboundMessage{Channel: "cli", ChatID: "1", Content: "two"}); err != nil {
		t.Fatal(err)
	}

	if len(p.requests) != 2 {
		t.Fatalf("requests = %d", len(p.requests))
	}
	// First request: system + user.
	first := p.requests[0].Messages
	if len(first) != 2 || first[0].Role != core.RoleSystem || first[0].Content.Text != "SYSTEM PROMPT" {
		t.Errorf("first request wrong: %+v", first)
	}
	// Second request must replay the prior turn: system, user, assistant, user.
	second := p.requests[1].Messages
	if len(second) != 4 {
		t.Fatalf("second request has %d messages, want 4: %+v", len(second), second)
	}
	if second[0].Role != core.RoleSystem {
		t.Errorf("system not first: %q", second[0].Role)
	}
	if second[1].Content.Text != "one" || second[2].Content.Text != "first" || second[3].Content.Text != "two" {
		t.Errorf("history not replayed correctly: %q %q %q",
			second[1].Content.Text, second[2].Content.Text, second[3].Content.Text)
	}
}

func TestLoopPersistsUserMessageBeforeRun(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{err: errors.New("boom")}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p

	// The run fails, but the user's input must already be persisted.
	_, err := l.ProcessMessage(context.Background(), core.InboundMessage{
		Channel: "cli", ChatID: "1", Content: "important input",
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := store.get("cli:1")
	if tr == nil || len(tr.messages) == 0 {
		t.Fatal("user message was not persisted before the run")
	}
	if tr.messages[0].Content.Text != "important input" {
		t.Errorf("persisted %q", tr.messages[0].Content.Text)
	}
}

func TestLoopNewCommandClearsSession(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "hi", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p
	ctx := context.Background()

	if _, err := l.ProcessMessage(ctx, core.InboundMessage{Channel: "cli", ChatID: "1", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(store.get("cli:1").messages) != 2 {
		t.Fatal("setup failed")
	}

	out, err := l.ProcessMessage(ctx, core.InboundMessage{Channel: "cli", ChatID: "1", Content: "/new"})
	if err != nil {
		t.Fatal(err)
	}
	// Exact reference text (builtin.py:343).
	if out.Content != "New session started." {
		t.Errorf("got %q", out.Content)
	}
	if len(store.get("cli:1").messages) != 0 {
		t.Errorf("session not cleared: %+v", store.get("cli:1").messages)
	}
	// /new must not reach the model.
	if len(p.requests) != 1 {
		t.Errorf("model called for /new: %d requests", len(p.requests))
	}
}

func TestLoopHelpCommand(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p

	out, err := l.ProcessMessage(context.Background(), core.InboundMessage{
		Channel: "cli", ChatID: "1", Content: "/help",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "/new") || !strings.Contains(out.Content, "/help") {
		t.Errorf("help text = %q", out.Content)
	}
	if len(p.requests) != 0 {
		t.Error("model called for /help")
	}
}

// TestLoopUnknownCommandRejected pins router.py:85-100: an unrecognised slash
// command must NOT be forwarded to the model.
func TestLoopUnknownCommandRejected(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p

	out, err := l.ProcessMessage(context.Background(), core.InboundMessage{
		Channel: "cli", ChatID: "1", Content: "/bogus arg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Unknown command") {
		t.Errorf("got %q", out.Content)
	}
	if len(p.requests) != 0 {
		t.Error("unknown command reached the model")
	}
}

func TestLoopToolExecution(t *testing.T) {
	store := newFakeStore()
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})
	p := &scriptedProvider{responses: []*core.Response{
		{FinishReason: core.FinishToolCalls, ToolCalls: []core.ToolCall{toolCall("c1", "echo", `{"v":"z"}`)}},
		{Content: "final", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, reg)
	l.cfg.Provider = p

	out, err := l.ProcessMessage(context.Background(), core.InboundMessage{
		Channel: "cli", ChatID: "1", Content: "use a tool",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "final" {
		t.Errorf("content = %q", out.Content)
	}

	tr := store.get("cli:1")
	// user, assistant(tool_calls), tool, assistant(final)
	if len(tr.messages) != 4 {
		t.Fatalf("transcript = %d, want 4: %+v", len(tr.messages), tr.messages)
	}
	if tr.messages[1].Role != core.RoleAssistant || len(tr.messages[1].ToolCalls) != 1 {
		t.Errorf("assistant tool_calls not persisted: %+v", tr.messages[1])
	}
	toolMsg := tr.messages[2]
	if toolMsg.Role != core.RoleTool {
		t.Errorf("tool message role = %q", toolMsg.Role)
	}
	// The reference emits name on tool messages (runner.py:533-543).
	if toolMsg.Name != "echo" {
		t.Errorf("tool message name = %q, want echo", toolMsg.Name)
	}
	if tr.messages[3].Content.Text != "final" {
		t.Errorf("final not persisted: %+v", tr.messages[3])
	}
}

func TestLoopHandlePublishesOutbound(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "published", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p
	ctx := context.Background()

	if err := l.Handle(ctx, core.InboundMessage{Channel: "cli", ChatID: "9", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.cfg.Bus.ConsumeOutbound(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "published" || got.ChatID != "9" {
		t.Errorf("outbound = %+v", got)
	}
}

func TestLoopRunConsumesUntilBusClosed(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "one", FinishReason: core.FinishStop},
		{Content: "two", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p
	ctx := context.Background()

	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	for _, msg := range []string{"a", "b"} {
		if err := l.cfg.Bus.PublishInbound(ctx, core.InboundMessage{
			Channel: "cli", ChatID: "1", Content: msg}); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for both outbound replies.
	for i := 0; i < 2; i++ {
		if _, err := l.cfg.Bus.ConsumeOutbound(ctx); err != nil {
			t.Fatal(err)
		}
	}
	l.cfg.Bus.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after bus close")
	}
}

func TestLoopSeparateSessionsIsolated(t *testing.T) {
	store := newFakeStore()
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "r1", FinishReason: core.FinishStop},
		{Content: "r2", FinishReason: core.FinishStop},
	}}
	l := newTestLoop(t, store, nil)
	l.cfg.Provider = p
	ctx := context.Background()

	if _, err := l.ProcessMessage(ctx, core.InboundMessage{Channel: "cli", ChatID: "A", Content: "msgA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ProcessMessage(ctx, core.InboundMessage{Channel: "cli", ChatID: "B", Content: "msgB"}); err != nil {
		t.Fatal(err)
	}

	a := store.get("cli:A")
	b := store.get("cli:B")
	if len(a.messages) != 2 || len(b.messages) != 2 {
		t.Fatalf("sessions not isolated: A=%d B=%d", len(a.messages), len(b.messages))
	}
	if a.messages[0].Content.Text != "msgA" || b.messages[0].Content.Text != "msgB" {
		t.Error("session contents crossed over")
	}
}

func TestNewLoopValidatesConfig(t *testing.T) {
	b := bus.New(bus.Options{})
	defer b.Close()
	store := newFakeStore()
	p := &scriptedProvider{}

	if _, err := NewLoop(LoopConfig{Store: store, Provider: p}); err == nil {
		t.Error("expected error for missing Bus")
	}
	if _, err := NewLoop(LoopConfig{Bus: b, Provider: p}); err == nil {
		t.Error("expected error for missing Store")
	}
	if _, err := NewLoop(LoopConfig{Bus: b, Store: store}); err == nil {
		t.Error("expected error for missing Provider")
	}
}

func TestNewLoopDefaults(t *testing.T) {
	b := bus.New(bus.Options{})
	defer b.Close()
	l, err := NewLoop(LoopConfig{Bus: b, Store: newFakeStore(), Provider: &scriptedProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if l.cfg.MaxIterations != DefaultMaxToolIterations {
		t.Errorf("MaxIterations = %d", l.cfg.MaxIterations)
	}
	if l.cfg.MaxToolResultChars != DefaultMaxToolResultChars {
		t.Errorf("MaxToolResultChars = %d", l.cfg.MaxToolResultChars)
	}
	if l.cfg.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d", l.cfg.MaxTokens)
	}
	if l.cfg.Temperature != 0.1 {
		t.Errorf("Temperature = %v", l.cfg.Temperature)
	}
	// AgentLoop hard-codes concurrent_tools=True (loop.py:1179).
	if !l.cfg.ConcurrentTools {
		t.Error("ConcurrentTools should default to true, matching loop.py:1179")
	}
}

func TestISOLocalFormat(t *testing.T) {
	// Kept in a helper so the loop test file stays readable.
	got := isoLocal(time.Date(2026, 9, 16, 2, 38, 34, 123456000, time.Local))
	if got != "2026-09-16T02:38:34.123456" {
		t.Errorf("isoLocal = %q", got)
	}
	got = isoLocal(time.Date(2026, 9, 16, 2, 38, 34, 0, time.Local))
	if got != "2026-09-16T02:38:34" {
		t.Errorf("isoLocal (no fraction) = %q", got)
	}
	if strings.ContainsAny(got, "Z+") {
		t.Errorf("timestamp must be naive local, got %q", got)
	}
}
