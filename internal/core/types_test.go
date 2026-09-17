package core

import (
	"encoding/json"
	"testing"
)

func TestContentRoundTripString(t *testing.T) {
	cases := []string{"", "hello", "multi\nline", `quote " and \ backslash`, "unicode: 日本語 émoji 🎉"}
	for _, in := range cases {
		raw, err := json.Marshal(TextContent(in))
		if err != nil {
			t.Fatalf("marshal %q: %v", in, err)
		}
		var got Content
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal %q: %v", in, err)
		}
		if !got.IsText() || got.Text != in {
			t.Errorf("round trip %q -> text=%v %q", in, got.IsText(), got.Text)
		}
	}
}

func TestContentNullAndBlocks(t *testing.T) {
	var c Content
	if err := json.Unmarshal([]byte("null"), &c); err != nil {
		t.Fatalf("null: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("null should be zero content, got %+v", c)
	}

	blocks := `[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]`
	var bc Content
	if err := json.Unmarshal([]byte(blocks), &bc); err != nil {
		t.Fatalf("blocks: %v", err)
	}
	if bc.IsText() {
		t.Error("blocks should not be text content")
	}
	if len(bc.Blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(bc.Blocks))
	}
	if bc.Blocks[0].Type != "text" || bc.Blocks[0].Text != "hi" {
		t.Errorf("block 0 wrong: %+v", bc.Blocks[0])
	}
	if bc.Blocks[1].Type != "image_url" {
		t.Errorf("block 1 type wrong: %q", bc.Blocks[1].Type)
	}
}

// TestContentRejectsScalar ensures a bare number/bool is rejected rather than
// silently coerced, matching the strictness of the Python parse path.
func TestContentRejectsScalar(t *testing.T) {
	for _, in := range []string{"42", "true", `"unterminated`} {
		var c Content
		if err := json.Unmarshal([]byte(in), &c); err == nil {
			t.Errorf("expected error for %s, got %+v", in, c)
		}
	}
}

func TestMessageRoundTripPreservesUnknownKeys(t *testing.T) {
	in := `{"role":"assistant","content":"hi","timestamp":"2026-09-16T02:38:34.123456","tool_calls":[{"id":"a","name":"f","arguments":"{}"}],"_hidden_history":true,"custom_field":{"nested":[1,2,3]}}`

	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Role != RoleAssistant {
		t.Errorf("role = %q", m.Role)
	}
	if !m.Content.IsText() || m.Content.Text != "hi" {
		t.Errorf("content = %+v", m.Content)
	}
	if m.Timestamp != "2026-09-16T02:38:34.123456" {
		t.Errorf("timestamp = %q", m.Timestamp)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name != "f" {
		t.Errorf("tool_calls = %+v", m.ToolCalls)
	}
	if !m.IsHiddenHistory() {
		t.Error("expected hidden history")
	}
	if _, ok := m.Extra("custom_field"); !ok {
		t.Error("custom_field lost")
	}

	out, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Semantic equality: parse both and compare as generic JSON. Key order is
	// intentionally not required to match, since Python parses into a dict.
	var a, b any
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatalf("reparse: %v (%s)", err, out)
	}
	as, _ := json.Marshal(a)
	bs, _ := json.Marshal(b)
	if string(as) != string(bs) {
		t.Errorf("semantic round trip mismatch:\n in: %s\nout: %s", as, bs)
	}
}

func TestMessageKeyOrderMatchesPython(t *testing.T) {
	m := NewMessage(RoleUser, "hello")
	m.Timestamp = "2026-09-16T02:38:34"
	m.SetExtra("_hidden_history", json.RawMessage("true"))

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"user","content":"hello","timestamp":"2026-09-16T02:38:34","_hidden_history":true}`
	if string(out) != want {
		t.Errorf("key order mismatch:\n got: %s\nwant: %s", out, want)
	}
}

func TestMessageToolRole(t *testing.T) {
	in := `{"role":"tool","content":"result","tool_call_id":"call_1","name":"read_file"}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != RoleTool || m.ToolCallID != "call_1" || m.Name != "read_file" {
		t.Errorf("got %+v", m)
	}
	out, _ := json.Marshal(&m)
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id lost: %s", out)
	}
}

func TestMessageNullContentPreserved(t *testing.T) {
	// Python can persist assistant messages with content=None.
	in := `{"role":"assistant","content":null,"tool_calls":[]}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if v, ok := back["content"]; !ok || v != nil {
		t.Errorf("null content not preserved: %s", out)
	}
}

func TestResponseShouldExecuteTools(t *testing.T) {
	cases := []struct {
		finish FinishReason
		calls  int
		want   bool
	}{
		{FinishToolCalls, 1, true},
		{FinishStop, 1, true},
		{FinishFunctionCall, 1, true},
		{FinishStop, 0, false},
		{FinishContentFilter, 1, false},
		{FinishRefusal, 1, false},
		{FinishError, 1, false},
	}
	for _, c := range cases {
		r := &Response{FinishReason: c.finish}
		for i := 0; i < c.calls; i++ {
			r.ToolCalls = append(r.ToolCalls, ToolCall{ID: "x", Name: "f"})
		}
		if got := r.ShouldExecuteTools(); got != c.want {
			t.Errorf("finish=%s calls=%d: got %v want %v", c.finish, c.calls, got, c.want)
		}
	}
}

func TestInboundSessionKey(t *testing.T) {
	m := &InboundMessage{Channel: "telegram", ChatID: "42"}
	if got := m.SessionKey(); got != "telegram:42" {
		t.Errorf("got %q", got)
	}
	override := "custom:key"
	m.SessionKeyOverride = &override
	if got := m.SessionKey(); got != "custom:key" {
		t.Errorf("override got %q", got)
	}
	if !m.IsUserInput() {
		t.Error("telegram should be user input")
	}
	sys := &InboundMessage{Channel: "system"}
	if sys.IsUserInput() {
		t.Error("system channel should not be user input")
	}
}

func TestToolCallArgumentsString(t *testing.T) {
	tc := &ToolCall{}
	if got := tc.ArgumentsString(); got != "{}" {
		t.Errorf("empty args = %q, want {}", got)
	}
	tc.Arguments = json.RawMessage(`{"a":1}`)
	if got := tc.ArgumentsString(); got != `{"a":1}` {
		t.Errorf("args = %q", got)
	}
	if tc.HasValidName() {
		t.Error("empty name should be invalid")
	}
	tc.Name = "f"
	if !tc.HasValidName() {
		t.Error("name should be valid")
	}
}
