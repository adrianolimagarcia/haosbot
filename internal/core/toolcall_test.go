package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolCallUnmarshalAcceptsNestedShape is the trap this method closes: the
// OpenAI wire format nests name and arguments under "function", and Go's
// default decoder ignores unknown keys, so the call decoded to a zero value
// with no name and no arguments — silently.
func TestToolCallUnmarshalAcceptsNestedShape(t *testing.T) {
	raw := `{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}`

	var tc ToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.ID != "c1" {
		t.Errorf("ID = %q, want c1", tc.ID)
	}
	if tc.Name != "read_file" {
		t.Errorf("Name = %q, want read_file (the nested name was ignored)", tc.Name)
	}
	if string(tc.Arguments) != `"{\"path\":\"a.txt\"}"` {
		t.Errorf("Arguments = %s, want the raw string as received", tc.Arguments)
	}
}

// TestToolCallUnmarshalKeepsFlatShape verifies the storage format still works.
func TestToolCallUnmarshalKeepsFlatShape(t *testing.T) {
	raw := `{"id":"c1","name":"list_dir","arguments":{"path":"."}}`

	var tc ToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Name != "list_dir" {
		t.Errorf("Name = %q, want list_dir", tc.Name)
	}
	if string(tc.Arguments) != `{"path":"."}` {
		t.Errorf("Arguments = %s", tc.Arguments)
	}
}

// TestToolCallUnmarshalNestedWins pins the precedence when a payload carries
// both shapes, matching the reference's _normalize_tool_call.
func TestToolCallUnmarshalNestedWins(t *testing.T) {
	raw := `{"id":"c1","name":"flat_name","arguments":{"a":1},` +
		`"function":{"name":"nested_name","arguments":{"b":2}}}`

	var tc ToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Name != "nested_name" {
		t.Errorf("Name = %q, want nested_name", tc.Name)
	}
	if string(tc.Arguments) != `{"b":2}` {
		t.Errorf("Arguments = %s, want the nested value", tc.Arguments)
	}
}

// TestToolCallUnmarshalKeepsArgumentsAsJSONValue pins this package's contract:
// arguments keep their JSON *value*, including the distinction between a string
// and an object. Unwrapping a string-encoded payload is the session layer's
// job, not this one's.
//
// Byte equality is deliberately not asserted: json.Marshal compacts a
// json.RawMessage field, so "{ }" is written as "{}". That is Go's documented
// behaviour for RawMessage, not a round-trip defect.
func TestToolCallUnmarshalKeepsArgumentsAsJSONValue(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// kind is the JSON kind the arguments must still have afterwards.
		kind string
	}{
		{"string payload", `{"id":"c","name":"n","arguments":"{}"}`, "string"},
		{"object payload", `{"id":"c","name":"n","arguments":{ }}`, "object"},
		{"nested object", `{"id":"c","name":"n","arguments": { "a" : [1, 2] } }`, "object"},
		{"null payload", `{"id":"c","name":"n","arguments":null}`, "empty"},
		{"absent payload", `{"id":"c","name":"n"}`, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var call ToolCall
			if err := json.Unmarshal([]byte(tc.raw), &call); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := jsonKind(call.Arguments); got != tc.kind {
				t.Errorf("arguments kind = %s, want %s (value %s)", got, tc.kind, call.Arguments)
			}

			// A second round trip must not change the kind either. This is
			// what the null-folding above buys: nil marshals to null, and
			// null decodes back to nil rather than to a 4-byte token.
			out, err := json.Marshal(call)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back ToolCall
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatalf("re-unmarshal %s: %v", out, err)
			}
			if got := jsonKind(back.Arguments); got != tc.kind {
				t.Errorf("arguments kind after a round trip = %s, want %s", got, tc.kind)
			}
		})
	}
}

// jsonKind reports the JSON kind of a raw value: string, object, array,
// number, bool, null, or empty when there is nothing at all.
func jsonKind(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "empty"
	}
	switch trimmed[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// TestToolCallUnmarshalReadsProviderSpecificFields covers both levels, because
// different providers attach them in different places.
func TestToolCallUnmarshalReadsProviderSpecificFields(t *testing.T) {
	raw := `{"id":"c1","name":"n","arguments":{},` +
		`"extra_content":{"thought":"x"},` +
		`"provider_specific_fields":{"top":"y"},` +
		`"function":{"name":"n","arguments":{},"provider_specific_fields":{"inner":"z"}}}`

	var tc ToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.ExtraContent["thought"] != "x" {
		t.Errorf("ExtraContent = %v", tc.ExtraContent)
	}
	if tc.ProviderSpecificFields["top"] != "y" {
		t.Errorf("ProviderSpecificFields = %v", tc.ProviderSpecificFields)
	}
	if tc.FunctionProviderSpecific["inner"] != "z" {
		t.Errorf("FunctionProviderSpecific = %v", tc.FunctionProviderSpecific)
	}
}

// TestMessageUnmarshalReadsNestedToolCalls is the end-to-end version: the
// message decoder must produce usable calls without a caller-side fixup.
func TestMessageUnmarshalReadsNestedToolCalls(t *testing.T) {
	raw := `{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}},` +
		`{"id":"c2","type":"function","function":{"name":"list_dir","arguments":"{}"}}]}`

	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(m.ToolCalls))
	}
	for i, want := range []string{"read_file", "list_dir"} {
		if m.ToolCalls[i].Name != want {
			t.Errorf("call %d name = %q, want %q", i, m.ToolCalls[i].Name, want)
		}
		if !m.ToolCalls[i].HasValidName() {
			t.Errorf("call %d has no valid name; it would be dropped on replay", i)
		}
	}
}

// TestToolCallUnmarshalRejectsNonObject verifies a malformed entry is an error
// rather than a silent zero value.
func TestToolCallUnmarshalRejectsNonObject(t *testing.T) {
	for _, raw := range []string{`"a string"`, `[1,2]`, `42`} {
		var tc ToolCall
		if err := json.Unmarshal([]byte(raw), &tc); err == nil {
			t.Errorf("%s: expected an error, got %+v", raw, tc)
		}
	}
}

// TestNormalizeToolArguments covers the shared helper directly.
func TestNormalizeToolArguments(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{`"{\"a\":1}"`, `{"a":1}`},   // string-encoded object is unwrapped
		{`"not json"`, `"not json"`}, // non-JSON string is kept as the payload
		{`"  "`, `"  "`},             // blank string is kept
		{``, ``},
	}
	for _, tc := range cases {
		got := string(NormalizeToolArguments(json.RawMessage(tc.in)))
		if got != tc.want {
			t.Errorf("NormalizeToolArguments(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := NormalizeToolArguments(nil); got != nil {
		t.Errorf("nil input = %s, want nil", got)
	}
}
