package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Message is one entry of a conversation transcript.
//
// Mirrors the Python message dicts built by Session.add_message
// (upstream nanobot/session/manager.py:313) and consumed by
// Session.get_history (upstream nanobot/session/manager.py:438).
//
// Python emits keys in insertion order: role, content, timestamp, then any
// extra kwargs. Go maps do not preserve order, so Message keeps unknown keys
// in an ordered list. This guarantees that a transcript written by nanobot-go
// round-trips without losing or reordering fields the Go code does not model.
type Message struct {
	Role    Role
	Content Content

	// Timestamp is the raw ISO-8601 string as written by Python
	// (datetime.now().isoformat()), which is local time with NO timezone
	// suffix. It is kept as a string rather than time.Time to preserve the
	// exact representation on round trip.
	Timestamp string

	// Known optional fields.
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ThinkingBlocks   []map[string]any `json:"thinking_blocks,omitempty"`

	// extra holds every key not modelled above, in original order.
	extra []kv
	// present records which known optional fields were explicitly present in
	// the source JSON, so that an explicit empty value is not silently
	// dropped on round trip.
	present map[string]bool
}

type kv struct {
	key string
	val json.RawMessage
}

// orderedKeys are emitted first, in this order, matching Python insertion order.
var orderedKeys = []string{"role", "content", "timestamp"}

// typedKeys are the optional fields this package models.
var typedKeys = []string{
	"tool_calls", "tool_call_id", "name", "reasoning_content", "thinking_blocks",
}

// NewMessage builds a message with a role and text content.
func NewMessage(role Role, text string) *Message {
	return &Message{Role: role, Content: TextContent(text)}
}

// Extra returns the value of an unmodelled key.
func (m *Message) Extra(key string) (json.RawMessage, bool) {
	for _, e := range m.extra {
		if e.key == key {
			return e.val, true
		}
	}
	return nil, false
}

// SetExtra sets an unmodelled key, preserving first-insertion order.
func (m *Message) SetExtra(key string, val json.RawMessage) {
	// A MODELLED key must be written to its struct field. Storing it in the
	// unknown-key list would be silently discarded by MarshalJSON, which reads
	// modelled keys from their fields — that is exactly the defect that made
	// every outgoing tool message carry an empty tool_call_id.
	if m.setTyped(key, val) {
		return
	}
	for i := range m.extra {
		if m.extra[i].key == key {
			m.extra[i].val = val
			return
		}
	}
	m.extra = append(m.extra, kv{key: key, val: val})
	if m.present == nil {
		m.present = map[string]bool{}
	}
	m.present[key] = true
}

// setTyped routes a modelled optional key to its struct field. It reports
// whether the key was modelled, so callers cannot silently shadow a field.
func (m *Message) setTyped(key string, val json.RawMessage) bool {
	var ok bool
	switch key {
	case "tool_calls":
		var v []ToolCall
		ok = json.Unmarshal(val, &v) == nil
		if ok {
			m.ToolCalls = v
		}
	case "tool_call_id":
		var v string
		ok = json.Unmarshal(val, &v) == nil
		if ok {
			m.ToolCallID = v
		}
	case "name":
		var v string
		ok = json.Unmarshal(val, &v) == nil
		if ok {
			m.Name = v
		}
	case "reasoning_content":
		var v string
		ok = json.Unmarshal(val, &v) == nil
		if ok {
			m.ReasoningContent = v
		}
	case "thinking_blocks":
		var v []map[string]any
		ok = json.Unmarshal(val, &v) == nil
		if ok {
			m.ThinkingBlocks = v
		}
	default:
		return false
	}
	if ok {
		if m.present == nil {
			m.present = map[string]bool{}
		}
		m.present[key] = true
	}
	return true
}

// DeleteExtra removes an unmodelled key.
func (m *Message) DeleteExtra(key string) {
	for i := range m.extra {
		if m.extra[i].key == key {
			m.extra = append(m.extra[:i], m.extra[i+1:]...)
			break
		}
	}
	delete(m.present, key)
}

// MarshalJSON writes the message in Python's key order.
func (m *Message) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	writeField := func(key string, raw []byte) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(raw)
	}

	writeField("role", mustJSON(string(m.Role)))

	if m.Content.IsZero() && !m.present["content"] {
		writeField("content", []byte("null"))
	} else {
		c, err := json.Marshal(m.Content)
		if err != nil {
			return nil, fmt.Errorf("core: marshal content: %w", err)
		}
		writeField("content", c)
	}

	if m.Timestamp != "" {
		writeField("timestamp", mustJSON(m.Timestamp))
	}

	// Typed optional fields, in Python's order.
	if m.ToolCalls != nil || m.present["tool_calls"] {
		v := m.ToolCalls
		if v == nil {
			v = []ToolCall{}
		}
		writeField("tool_calls", mustJSON(v))
	}
	if m.ToolCallID != "" || m.present["tool_call_id"] {
		writeField("tool_call_id", mustJSON(m.ToolCallID))
	}
	if m.Name != "" || m.present["name"] {
		writeField("name", mustJSON(m.Name))
	}
	if m.ReasoningContent != "" || m.present["reasoning_content"] {
		writeField("reasoning_content", mustJSON(m.ReasoningContent))
	}
	if m.ThinkingBlocks != nil || m.present["thinking_blocks"] {
		writeField("thinking_blocks", mustJSON(m.ThinkingBlocks))
	}

	// Remaining unmodelled keys, in original insertion order.
	emitted := map[string]bool{}
	for _, k := range orderedKeys {
		emitted[k] = true
	}
	for _, k := range typedKeys {
		emitted[k] = true
	}
	for _, e := range m.extra {
		if emitted[e.key] {
			continue
		}
		writeField(e.key, e.val)
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON parses a transcript message, retaining unknown keys.
func (m *Message) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("core: message must be a JSON object")
	}

	m.extra = nil
	m.present = map[string]bool{}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("core: message key must be a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		m.present[key] = true

		switch key {
		case "role":
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("core: message.role: %w", err)
			}
			m.Role = Role(s)
		case "content":
			var c Content
			if err := json.Unmarshal(raw, &c); err != nil {
				return fmt.Errorf("core: message.content: %w", err)
			}
			m.Content = c
		case "timestamp":
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				m.Timestamp = s
			} else {
				m.extra = append(m.extra, kv{key: key, val: raw})
			}
		case "tool_calls":
			if err := json.Unmarshal(raw, &m.ToolCalls); err != nil {
				m.extra = append(m.extra, kv{key: key, val: raw})
			}
		case "tool_call_id":
			if err := json.Unmarshal(raw, &m.ToolCallID); err != nil {
				m.extra = append(m.extra, kv{key: key, val: raw})
			}
		case "name":
			if err := json.Unmarshal(raw, &m.Name); err != nil {
				m.extra = append(m.extra, kv{key: key, val: raw})
			}
		case "reasoning_content":
			if err := json.Unmarshal(raw, &m.ReasoningContent); err != nil {
				m.extra = append(m.extra, kv{key: key, val: raw})
			}
		case "thinking_blocks":
			if err := json.Unmarshal(raw, &m.ThinkingBlocks); err != nil {
				m.extra = append(m.extra, kv{key: key, val: raw})
			}
		default:
			m.extra = append(m.extra, kv{key: key, val: raw})
		}
	}
	// consume closing brace
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// IsHiddenHistory reports whether the message is marked hidden from replay.
// Mirrors nanobot/session/history_visibility.py.
func (m *Message) IsHiddenHistory() bool {
	v, ok := m.Extra("_hidden_history")
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err == nil {
		return b
	}
	return false
}

// SortedExtraKeys returns unmodelled keys sorted, for deterministic output.
func (m *Message) SortedExtraKeys() []string {
	keys := make([]string, 0, len(m.extra))
	for _, e := range m.extra {
		keys = append(keys, e.key)
	}
	sort.Strings(keys)
	return keys
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable for types that cannot fail to marshal here.
		return []byte("null")
	}
	return b
}
