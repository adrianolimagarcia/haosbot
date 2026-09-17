package runtimecontext

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// decodeJSON decodes a JSON object the way a session record is decoded:
// UseNumber, so an integer stays distinguishable from a float.
func decodeJSON(t *testing.T, doc string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	return out
}

// TestPublicHistoryMessageSuffix pins the string branch of
// public_history_message (runtime_context.py:215).
//
// The marker is removed in every case, even when nothing was stripped: the
// reference pops it before deciding whether the content is recoverable.
func TestPublicHistoryMessageSuffix(t *testing.T) {
	const suffix = "[Runtime Context]"
	marker := `{"version": 1, "suffix": "[Runtime Context]"}`

	cases := []struct {
		name    string
		doc     string
		content any
	}{
		{"tail_removed", `{"role":"user","content":"hi\n\n` + suffix + `","_runtime_context":` + marker + `}`, "hi"},
		{"exact_removed", `{"role":"user","content":"` + suffix + `","_runtime_context":` + marker + `}`, ""},
		{"absent_from_content", `{"role":"user","content":"hi","_runtime_context":` + marker + `}`, "hi"},
		{"single_newline_kept", `{"role":"user","content":"hi\n` + suffix + `","_runtime_context":` + marker + `}`, "hi\n" + suffix},
		{"empty_suffix", `{"role":"user","content":"hi\n\n` + suffix + `","_runtime_context":{"version":1,"suffix":""}}`, "hi\n\n" + suffix},
		{"non_string_suffix", `{"role":"user","content":"hi\n\n` + suffix + `","_runtime_context":{"version":1,"suffix":5}}`, "hi\n\n" + suffix},
		{"marker_removed_anyway", `{"role":"user","content":"hi","_runtime_context":"not a dict"}`, "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := decodeJSON(t, tc.doc)
			got := PublicHistoryMessage(message)
			if got["content"] != tc.content {
				t.Errorf("content = %#v, want %#v", got["content"], tc.content)
			}
			if _, present := got[HistoryMeta]; present {
				t.Error("marker key survived")
			}
			if _, present := message[HistoryMeta]; !present {
				t.Error("the caller's map was mutated")
			}
		})
	}
}

// TestPublicHistoryMessageVersionIsAValueComparison pins the trap in
// `marker_data.get("version") != 1`: Python compares VALUES, so True and 1.0
// are version 1 while the string "1" is not.
func TestPublicHistoryMessageVersionIsAValueComparison(t *testing.T) {
	const suffix = "[Runtime Context]"
	cases := []struct {
		version  string
		stripped bool
	}{
		{`1`, true},
		{`true`, true},
		{`1.0`, true},
		{`"1"`, false},
		{`2`, false},
		{`null`, false},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			doc := `{"role":"user","content":"hi\n\n` + suffix + `","_runtime_context":{"version":` +
				tc.version + `,"suffix":"` + suffix + `"}}`
			got := PublicHistoryMessage(decodeJSON(t, doc))
			want := any("hi\n\n" + suffix)
			if tc.stripped {
				want = "hi"
			}
			if got["content"] != want {
				t.Errorf("version %s: content = %#v, want %#v", tc.version, got["content"], want)
			}
		})
	}
}

// TestPublicHistoryMessageBlocks pins the list branch, including the
// number-by-value comparison (Python's 1 == 1.0) that a reflect.DeepEqual on
// json.Number would get wrong.
func TestPublicHistoryMessageBlocks(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		content any
	}{
		{
			"tail_removed",
			`{"role":"user","content":[{"type":"text","text":"real"},{"type":"text","text":"inj"}],` +
				`"_runtime_context":{"version":1,"blocks":[{"type":"text","text":"inj"}]}}`,
			[]any{map[string]any{"type": "text", "text": "real"}},
		},
		{
			"not_at_tail",
			`{"role":"user","content":[{"type":"text","text":"inj"},{"type":"text","text":"real"}],` +
				`"_runtime_context":{"version":1,"blocks":[{"type":"text","text":"inj"}]}}`,
			[]any{map[string]any{"type": "text", "text": "inj"}, map[string]any{"type": "text", "text": "real"}},
		},
		{
			"number_equality",
			`{"role":"user","content":[{"type":"text","n":1}],` +
				`"_runtime_context":{"version":1,"blocks":[{"type":"text","n":1.0}]}}`,
			[]any{},
		},
		{
			"blocks_longer_than_content",
			`{"role":"user","content":[{"type":"text","text":"inj"}],` +
				`"_runtime_context":{"version":1,"blocks":[{"type":"text","text":"inj"},{"type":"text","text":"x"}]}}`,
			[]any{map[string]any{"type": "text", "text": "inj"}},
		},
		{
			"empty_expected_blocks",
			`{"role":"user","content":[{"type":"text","text":"inj"}],` +
				`"_runtime_context":{"version":1,"blocks":[]}}`,
			[]any{map[string]any{"type": "text", "text": "inj"}},
		},
		{
			"content_is_a_string",
			`{"role":"user","content":"plain","_runtime_context":{"version":1,"blocks":[{"type":"text","text":"inj"}]}}`,
			"plain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PublicHistoryMessage(decodeJSON(t, tc.doc))
			if !reflect.DeepEqual(got["content"], tc.content) {
				t.Errorf("content = %#v, want %#v", got["content"], tc.content)
			}
		})
	}
}

// TestPublicHistoryMessagesPreservesOrderAndCount checks the batch wrapper is a
// map, not a filter: the reference returns one copy per input message.
func TestPublicHistoryMessagesPreservesOrderAndCount(t *testing.T) {
	in := []map[string]any{
		{"role": "user", "content": "a", HistoryMeta: map[string]any{"version": 1, "suffix": "S"}},
		{"role": "assistant", "content": "b"},
	}
	out := PublicHistoryMessages(in)
	if len(out) != 2 {
		t.Fatalf("got %d messages, want 2", len(out))
	}
	if out[0]["content"] != "a" || out[1]["content"] != "b" {
		t.Errorf("order changed: %#v", out)
	}
	if _, present := out[0][HistoryMeta]; present {
		t.Error("marker survived")
	}
	if _, present := in[0][HistoryMeta]; !present {
		t.Error("input message was mutated")
	}
}
