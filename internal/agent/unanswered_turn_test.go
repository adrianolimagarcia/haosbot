package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// A turn that ends without an answer must not leave its user message as the
// last entry of the transcript.
//
// The user message is persisted before the runner starts, but the turn's own
// messages were only persisted once the runner returned cleanly. A turn killed
// by the request deadline or by a cancellation therefore left the user message
// dangling, and the next turn saw an unanswered question, answered that stale
// question, and left the new one unanswered in turn. That is the failure the
// operator observed: a message asking "ola" was answered with the previous
// turn's fail2ban report.

func unansweredTurnMarker(t *testing.T, tr *fakeTranscript) core.Message {
	t.Helper()
	if tr == nil {
		t.Fatal("transcript was never opened")
	}
	if len(tr.messages) == 0 {
		t.Fatal("transcript is empty")
	}
	return tr.messages[len(tr.messages)-1]
}

func TestCancelledTurnClosesTheUserMessage(t *testing.T) {
	store := newFakeStore()
	l := newTestLoop(t, store, nil)
	msg := core.InboundMessage{Channel: "cli", ChatID: "stale-session", Content: "status do fail2ban"}
	key := msg.SessionKey()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := l.ProcessMessage(ctx, msg); err == nil {
		t.Fatal("cancelled turn returned no error")
	}

	tr := store.get(key)
	if tr == nil {
		t.Fatal("transcript was never opened")
	}
	if len(tr.messages) != 2 {
		t.Fatalf("transcript has %d messages, want user + marker: %#v", len(tr.messages), tr.messages)
	}
	if tr.messages[0].Role != core.RoleUser {
		t.Fatalf("first message role = %q, want user", tr.messages[0].Role)
	}
	last := unansweredTurnMarker(t, tr)
	if last.Role != core.RoleAssistant {
		t.Fatalf("last message role = %q, want assistant (the turn must close itself)", last.Role)
	}
	if !strings.Contains(last.Content.Text, "without an answer") {
		t.Fatalf("marker text = %q, want it to record the missing answer", last.Content.Text)
	}
	// The reason must survive so the operator can tell a deadline from a
	// cancellation after the fact.
	if !strings.Contains(last.Content.Text, context.Canceled.Error()) {
		t.Fatalf("marker text = %q, want it to name the cause %q", last.Content.Text, context.Canceled)
	}
}

// The marker must be bound to the turn it closes, so a later reader can tell
// which user message it answered.
func TestCancelledTurnMarkerCarriesTheTurnID(t *testing.T) {
	store := newFakeStore()
	l := newTestLoop(t, store, nil)
	msg := core.InboundMessage{Channel: "cli", ChatID: "stale-session", Content: "status do fail2ban"}
	key := msg.SessionKey()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.ProcessMessage(ctx, msg); err == nil {
		t.Fatal("cancelled turn returned no error")
	}

	tr := store.get(key)
	last := unansweredTurnMarker(t, tr)
	if last.Role != core.RoleAssistant {
		t.Fatalf("last message role = %q, want the assistant marker", last.Role)
	}
	raw, ok := last.Extra("turn_id")
	if !ok {
		t.Fatal("marker has no turn_id")
	}
	userRaw, ok := tr.messages[0].Extra("turn_id")
	if !ok {
		t.Fatal("user message has no turn_id")
	}
	if string(raw) != string(userRaw) {
		t.Fatalf("marker turn_id = %s, want the user message's %s", raw, userRaw)
	}
}

// The regression that matters to the operator: after a turn dies unanswered,
// the next turn must not be handed the stale question as an open one.
func TestNextTurnDoesNotReanswerAClosedTurn(t *testing.T) {
	store := newFakeStore()
	l := newTestLoop(t, store, nil)
	p := &scriptedProvider{responses: []*core.Response{
		{Content: "Olá! Como posso ajudar?", FinishReason: core.FinishStop},
	}}
	l.cfg.Provider = p

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.ProcessMessage(dead, core.InboundMessage{
		Channel: "cli", ChatID: "stale-session", Content: "status do fail2ban"}); err == nil {
		t.Fatal("cancelled turn returned no error")
	}

	if _, err := l.ProcessMessage(context.Background(), core.InboundMessage{
		Channel: "cli", ChatID: "stale-session", Content: "ola"}); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("provider was never called")
	}
	sent := p.requests[len(p.requests)-1].Messages
	for i, m := range sent {
		if m.Role != core.RoleUser || m.Content.Text != "status do fail2ban" {
			continue
		}
		if i+1 >= len(sent) {
			t.Fatal("the stale question is the last message sent to the provider")
		}
		if sent[i+1].Role != core.RoleAssistant {
			t.Fatalf("message after the stale question has role %q, want assistant", sent[i+1].Role)
		}
		return
	}
	t.Fatal("the stale question is missing from the request")
}

// A turn that dies must not be recorded as a success: the transcript has to
// show that the question went unanswered rather than silently dropping it.
func TestCancelledTurnIsNotRecordedAsAnAnswer(t *testing.T) {
	store := newFakeStore()
	l := newTestLoop(t, store, nil)
	msg := core.InboundMessage{Channel: "cli", ChatID: "stale-session", Content: "status do fail2ban"}
	key := msg.SessionKey()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.ProcessMessage(ctx, msg); err == nil {
		t.Fatal("cancelled turn returned no error")
	}

	tr := store.get(key)
	if tr.saves < 2 {
		t.Fatalf("transcript saved %d times, want the user message and the marker persisted", tr.saves)
	}
	if len(tr.messages) > 2 {
		t.Fatalf("cancelled turn persisted %d messages, want only user + marker", len(tr.messages))
	}
}
