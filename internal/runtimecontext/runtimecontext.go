// Package runtimecontext ports the user-visible-history helpers from
// nanobot/runtime_context.py (@1bb712d3): the trusted runtime-context marker
// constant and the two functions that strip it back out of a persisted
// transcript.
//
// SCOPE — this package is deliberately partial. Only these three symbols are
// ported:
//
//	RUNTIME_CONTEXT_HISTORY_META -> HistoryMeta
//	public_history_message       -> PublicHistoryMessage
//	public_history_messages      -> PublicHistoryMessages
//
// The injection side of runtime_context.py (RuntimeContextBlock, the
// build/append helpers, the WebUI quote metadata and the provider
// conversation-state plumbing) is NOT ported and is not in scope here.
//
// Messages are `map[string]any` rather than core.Message because these
// functions are defined over persisted message dicts, and a decoded JSON object
// is the only Go representation that can hold every value the reference's
// dicts hold: core.Message.Timestamp is a string, so the reference's own
// int-timestamp fixture (tests/agent/test_memory_store.py:535) could not even
// be decoded into it. The marker itself survives a session round trip as a
// message Extra (see internal/session/session_test.go), so a caller holding
// core.Message values converts through the JSON it already round-trips.
package runtimecontext

import (
	"encoding/json"
	"strings"
)

// HistoryMeta is RUNTIME_CONTEXT_HISTORY_META (runtime_context.py:14): the
// message key holding the trusted runtime-context marker.
const HistoryMeta = "_runtime_context"

// markerVersion is the only marker version the reference understands
// (runtime_context.py:227).
const markerVersion = 1

// PublicHistoryMessage returns a user-visible copy of a persisted message with
// the trusted runtime context removed exactly.
//
// Mirrors public_history_message (runtime_context.py:215-243). The copy is
// shallow: the reference deepcopies, but nothing below the top level is ever
// mutated here (both branches replace the top-level "content" key), so a
// shallow copy is observably identical.
//
// Two reference details are easy to miss and are reproduced:
//
//   - The version test is `marker_data.get("version") != 1`, which is a Python
//     *value* comparison: True and 1.0 both equal 1, so {"version": true} is
//     treated as version 1 and its suffix IS stripped. A marker with no
//     version key at all (None) is not version 1 and is left alone.
//   - The string branch returns early only when a non-empty string suffix
//     applies; otherwise control falls through to the block branch, where a
//     non-list content is left untouched.
func PublicHistoryMessage(message map[string]any) map[string]any {
	cleaned := make(map[string]any, len(message))
	for k, v := range message {
		cleaned[k] = v
	}

	// Python: cleaned.pop(RUNTIME_CONTEXT_HISTORY_META, None). The key is
	// removed whether or not it held a usable marker.
	marker := cleaned[HistoryMeta]
	delete(cleaned, HistoryMeta)

	markerData, ok := marker.(map[string]any)
	if !ok {
		return cleaned
	}
	if !isMarkerVersion(markerData["version"]) {
		return cleaned
	}

	content := cleaned["content"]
	if text, isText := content.(string); isText {
		if suffix, isSuffix := markerData["suffix"].(string); isSuffix && suffix != "" {
			switch {
			case text == suffix:
				cleaned["content"] = ""
			case strings.HasSuffix(text, "\n\n"+suffix):
				// The removed tail is exactly "\n\n"+suffix, so dropping that
				// many BYTES drops exactly the same characters Python's
				// character slice drops.
				cleaned["content"] = text[:len(text)-(len(suffix)+2)]
			}
			return cleaned
		}
	}

	expected, ok := markerData["blocks"].([]any)
	if !ok || len(expected) == 0 {
		return cleaned
	}
	blocks, ok := content.([]any)
	if !ok || len(blocks) < len(expected) {
		return cleaned
	}
	tail := blocks[len(blocks)-len(expected):]
	if !jsonValueEqual(tail, expected) {
		return cleaned
	}
	cleaned["content"] = blocks[:len(blocks)-len(expected)]
	return cleaned
}

// PublicHistoryMessages returns user-visible copies of persisted messages.
// Mirrors public_history_messages (runtime_context.py:246-248).
func PublicHistoryMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		out = append(out, PublicHistoryMessage(message))
	}
	return out
}

// isMarkerVersion reports whether v equals the integer 1 the way Python's
// `!= 1` does: True == 1 and 1.0 == 1, and nothing else.
func isMarkerVersion(v any) bool {
	switch n := v.(type) {
	case bool:
		return n
	case json.Number:
		f, err := n.Float64()
		return err == nil && f == markerVersion
	case float64:
		return n == markerVersion
	case int:
		return n == markerVersion
	}
	return false
}

// jsonValueEqual compares two decoded JSON values with Python's `==` for the
// value shapes json.Unmarshal produces.
//
// The only place this is observable is the block-tail comparison, where both
// sides come from the same session document. Numbers are compared by value
// because Python's 1 == 1.0 is True while json.Number("1") == json.Number("1.0")
// would not be; booleans compare as numbers for the same reason (True == 1).
func jsonValueEqual(a, b any) bool {
	if af, aok := asPyNumber(a); aok {
		bf, bok := asPyNumber(b)
		return bok && af == bf
	}
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonValueEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			other, present := bv[k]
			if !present || !jsonValueEqual(v, other) {
				return false
			}
		}
		return true
	}
	return false
}

// asPyNumber reports whether v is a number for Python's `==`, including the
// booleans (bool is a subclass of int).
func asPyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
