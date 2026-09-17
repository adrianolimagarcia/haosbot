package memory

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// TestFormatMessages pins the line shape of _format_messages (memory.py:642)
// for the cases the differential corpus also covers, so a failure here localises
// the problem without running Python.
func TestFormatMessages(t *testing.T) {
	cases := []struct {
		name     string
		messages []map[string]any
		want     string
	}{
		{
			"plain",
			[]map[string]any{{"role": "user", "content": "hi", "timestamp": "2026-07-27T12:00:00"}},
			"[2026-07-27T12:00] USER: hi",
		},
		{
			"missing_timestamp",
			[]map[string]any{{"role": "user", "content": "hi"}},
			"[?] USER: hi",
		},
		{
			"null_timestamp",
			[]map[string]any{{"role": "user", "content": "hi", "timestamp": nil}},
			"[?] USER: hi",
		},
		{
			"int_timestamp",
			[]map[string]any{{"role": "user", "content": "hi", "timestamp": json.Number("1720000000")}},
			"[1720000000] USER: hi",
		},
		{
			"bool_timestamp",
			[]map[string]any{{"role": "user", "content": "hi", "timestamp": true}},
			"[True] USER: hi",
		},
		{
			"float_timestamp",
			[]map[string]any{{"role": "user", "content": "hi", "timestamp": json.Number("1.5")}},
			"[1.5] USER: hi",
		},
		{
			"float_timestamp_whole",
			[]map[string]any{{"role": "user", "content": "hi", "timestamp": json.Number("1.0")}},
			"[1.0] USER: hi",
		},
		{
			"missing_role",
			[]map[string]any{{"content": "no role", "timestamp": "2026-07-28T12:00:00"}},
			"[2026-07-28T12:00] UNKNOWN: no role",
		},
		{
			"empty_role",
			[]map[string]any{{"role": "", "content": "x", "timestamp": "t"}},
			"[t] UNKNOWN: x",
		},
		{
			"tools_used",
			[]map[string]any{{"role": "assistant", "content": "hi", "timestamp": "t",
				"tools_used": []any{"read_file", "exec"}}},
			"[t] ASSISTANT [tools: read_file, exec]: hi",
		},
		{
			"tools_used_empty",
			[]map[string]any{{"role": "assistant", "content": "hi", "timestamp": "t",
				"tools_used": []any{}}},
			"[t] ASSISTANT: hi",
		},
		{
			"media_only_turn",
			[]map[string]any{{"role": "user", "content": "", "media": []any{"/m/a.png"},
				"timestamp": "2026-07-27"}},
			"[2026-07-27] USER: [image: /m/a.png]",
		},
		{
			"falsy_content_skipped",
			[]map[string]any{{"role": "user", "content": "", "timestamp": "t"}},
			"",
		},
		{
			"zero_content_skipped",
			[]map[string]any{{"role": "user", "content": json.Number("0"), "timestamp": "t"}},
			"",
		},
		{
			"skips_join_the_rest",
			[]map[string]any{
				{"role": "user", "content": "", "timestamp": "t1"},
				{"role": "tool", "content": "result", "timestamp": "t2"},
			},
			"[t2] TOOL: result",
		},
		{
			"empty_batch",
			nil,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatMessages(tc.messages); got != tc.want {
				t.Errorf("FormatMessages\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestFormatMessagesTimestampSlicesCharacters pins `timestamp[:16]`: a byte
// slice would cut a multi-byte timestamp in the middle of a character.
func TestFormatMessagesTimestampSlicesCharacters(t *testing.T) {
	stamp := strings.Repeat("\u65e5", 24) // 24 characters, 72 bytes
	got := FormatMessages([]map[string]any{{"role": "user", "content": "hi", "timestamp": stamp}})
	want := "[" + strings.Repeat("\u65e5", 16) + "] USER: hi"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "["+strings.Repeat("\u65e5", 16)+"]") {
		t.Error("the timestamp was cut by bytes")
	}
}

// TestFormatMessagesMultimodalContent documents the one shape this port renders
// differently from the reference.
//
// The reference stringifies a content list with Python's repr(); a Go map has
// no key order, so a nested object's keys are rendered sorted here. A
// single-key object is unaffected, which is why the differential corpus can
// still compare that shape exactly.
func TestFormatMessagesMultimodalContent(t *testing.T) {
	t.Run("single_key_object_matches_python", func(t *testing.T) {
		got := FormatMessages([]map[string]any{{"role": "user", "timestamp": "t",
			"content": []any{map[string]any{"type": "text"}}}})
		if want := "[t] USER: [{'type': 'text'}]"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("scalars_match_python", func(t *testing.T) {
		got := FormatMessages([]map[string]any{{"role": "user", "timestamp": "t",
			"content": []any{"a", "b"}}})
		if want := "[t] USER: ['a', 'b']"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("multi_key_object_uses_sorted_keys", func(t *testing.T) {
		got := FormatMessages([]map[string]any{{"role": "user", "timestamp": "t",
			"content": []any{map[string]any{"type": "text", "n": json.Number("1")}}}})
		// Python renders {'type': 'text', 'n': 1}: document order. This port
		// cannot recover it, so the keys are sorted.
		if want := "[t] USER: [{'n': 1, 'type': 'text'}]"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("repr_quotes_and_escapes", func(t *testing.T) {
		got := FormatMessages([]map[string]any{{"role": "user", "timestamp": "t",
			"content": []any{map[string]any{"a'b": "x\ny\tz\\w"}}}})
		if want := `[t] USER: [{"a'b": 'x\ny\tz\\w'}]`; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestPyStrNumbers pins str() for the JSON number literals a decoded session
// can hold.
func TestPyStrNumbers(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1720000000", "1720000000"},
		{"0", "0"},
		{"-0", "0"},
		{"1.0", "1.0"},
		{"1.5", "1.5"},
		{"1e3", "1000.0"},
		{"1e16", pyjson.FormatFloat(1e16)},
		{"100000000000000000000", "100000000000000000000"},
	}
	for _, tc := range cases {
		if got := pyStr(json.Number(tc.in)); got != tc.want {
			t.Errorf("pyStr(json.Number(%q)) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPyStrFalsyAndTruthy pins the values Python considers falsy, which decide
// whether a message line is emitted at all.
func TestPyStrFalsyAndTruthy(t *testing.T) {
	falsy := []any{nil, false, "", json.Number("0"), json.Number("0.0"), []any{}, map[string]any{}}
	for _, v := range falsy {
		if pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) = true, want false", v)
		}
	}
	truthy := []any{true, "x", " ", json.Number("1"), json.Number("-1"), []any{"a"},
		map[string]any{"a": 1}, json.Number("0.1")}
	for _, v := range truthy {
		if !pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) = false, want true", v)
		}
	}
}

// TestPyJoin pins `", ".join(value)`, including the Python quirk that a str is
// an iterable of characters.
func TestPyJoin(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{[]any{"a", "b"}, "a, b"},
		{[]any{"a"}, "a"},
		{[]any{}, ""},
		{"ab", "a, b"},
		{"", ""},
		{map[string]any{"b": 1, "a": 2}, "a, b"},
	}
	for _, tc := range cases {
		if got := pyJoin(", ", tc.in); got != tc.want {
			t.Errorf("pyJoin(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildRawCheckpoint pins the cap and the two normalisation passes.
func TestBuildRawCheckpoint(t *testing.T) {
	store := newStore(t)

	t.Run("empty", func(t *testing.T) {
		if got := store.BuildRawCheckpoint(nil, nil); got != "[RAW] 0 messages" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("over_default_cap", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": strings.Repeat("x", 20000),
			"timestamp": "t"}}
		got := store.BuildRawCheckpoint(messages, nil)
		if want := rawArchiveMaxChars + len(textutil.TruncatedSuffix); len([]rune(got)) != want {
			t.Errorf("checkpoint has %d characters, want %d", len([]rune(got)), want)
		}
		if !strings.HasSuffix(got, textutil.TruncatedSuffix) {
			t.Error("checkpoint is not marked truncated")
		}
	})

	t.Run("explicit_zero_disables_the_cap", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": strings.Repeat("x", 20000),
			"timestamp": "t"}}
		zero := 0
		got := store.BuildRawCheckpoint(messages, &zero)
		if strings.HasSuffix(got, textutil.TruncatedSuffix) {
			t.Error("an explicit max_chars=0 must disable the cap, not select the default")
		}
		if len([]rune(got)) != len("[RAW] 1 messages\n[t] USER: ")+20000 {
			t.Errorf("checkpoint length = %d", len([]rune(got)))
		}
	})

	t.Run("explicit_cap", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": "hello", "timestamp": "t"}}
		forty := 40
		got := store.BuildRawCheckpoint(messages, &forty)
		if want := "[RAW] 1 messages\n[t] USER: hello"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("think_tags_are_stripped", func(t *testing.T) {
		messages := []map[string]any{{"role": "assistant", "content": "<think>hidden</think>visible",
			"timestamp": "t"}}
		got := store.BuildRawCheckpoint(messages, nil)
		if want := "[RAW] 1 messages\n[t] ASSISTANT: visible"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("runtime_context_is_excluded", func(t *testing.T) {
		messages := []map[string]any{{
			"role": "user", "content": "what did I ask?\n\n[Runtime Context]", "timestamp": "t",
			"_runtime_context": map[string]any{"version": json.Number("1"), "suffix": "[Runtime Context]"},
		}}
		got := store.BuildRawCheckpoint(messages, nil)
		if want := "[RAW] 1 messages\n[t] USER: what did I ask?"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestRawArchivePersistsAndReturnsTheCheckpoint pins raw_archive: the returned
// string is exactly what landed in history.jsonl, under the session key.
func TestRawArchivePersistsAndReturnsTheCheckpoint(t *testing.T) {
	store := newStore(t)
	messages := []map[string]any{{"role": "user", "content": "hi", "timestamp": "t"}}

	checkpoint, err := store.RawArchive(messages, nil, "cli:test")
	if err != nil {
		t.Fatalf("RawArchive: %v", err)
	}
	if want := "[RAW] 1 messages\n[t] USER: hi"; checkpoint != want {
		t.Fatalf("checkpoint = %q, want %q", checkpoint, want)
	}

	entries, err := store.ReadEntries()
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0]["content"] != checkpoint {
		t.Errorf("persisted %q, returned %q", entries[0]["content"], checkpoint)
	}
	if entries[0]["session_key"] != "cli:test" {
		t.Errorf("session_key = %#v", entries[0]["session_key"])
	}
}

// TestRawArchiveWithoutSessionKey checks the key is omitted, not written empty.
func TestRawArchiveWithoutSessionKey(t *testing.T) {
	store := newStore(t)
	if _, err := store.RawArchive(nil, nil, ""); err != nil {
		t.Fatalf("RawArchive: %v", err)
	}
	raw, err := os.ReadFile(store.historyFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "session_key") {
		t.Errorf("history line carries a session_key: %s", raw)
	}
}

// TestRawArchiveThenAppendHistoryAppliesItsOwnCap pins the two-pass
// normalisation: raw_archive passes NO max_chars to append_history, so an
// explicit max_chars above the hard cap is still capped on write.
func TestRawArchiveThenAppendHistoryAppliesItsOwnCap(t *testing.T) {
	store := newStore(t)
	huge := 100000
	messages := []map[string]any{{"role": "user", "content": strings.Repeat("y", 70000), "timestamp": "t"}}

	checkpoint, err := store.RawArchive(messages, &huge, "cli:test")
	if err != nil {
		t.Fatalf("RawArchive: %v", err)
	}
	if len([]rune(checkpoint)) <= historyEntryHardCap {
		t.Fatalf("the checkpoint should exceed the hard cap before appending: %d", len([]rune(checkpoint)))
	}

	entries, err := store.ReadEntries()
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	persisted, _ := entries[0]["content"].(string)
	if want := historyEntryHardCap + len(textutil.TruncatedSuffix); len([]rune(persisted)) != want {
		t.Errorf("persisted entry has %d characters, want %d", len([]rune(persisted)), want)
	}
	if persisted == checkpoint {
		t.Error("append_history did not re-normalise the checkpoint")
	}
}

// TestFormatMessagesRequiresUseNumber documents the decoding contract: a
// plain json.Unmarshal turns a JSON integer into a float64, and str(float) is
// not str(int).
func TestFormatMessagesRequiresUseNumber(t *testing.T) {
	doc := `{"role":"user","content":"hi","timestamp":1720000000}`

	var plain map[string]any
	if err := json.Unmarshal([]byte(doc), &plain); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := FormatMessages([]map[string]any{plain}), "[1720000000.0] USER: hi"; got != want {
		t.Errorf("float64 decoding: got %q, want %q", got, want)
	}

	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var exact map[string]any
	if err := dec.Decode(&exact); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := FormatMessages([]map[string]any{exact}), "[1720000000] USER: hi"; got != want {
		t.Errorf("UseNumber decoding: got %q, want %q", got, want)
	}
}
