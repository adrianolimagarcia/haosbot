package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// Compile-time assertion that *Session satisfies the interface internal/agent
// consumes (internal/agent/loop.go:43).
var _ interface {
	Key() string
	Messages() []core.Message
	AddMessage(core.Message)
	Clear()
	Save() error
} = (*Session)(nil)

// Transcript is a local stand-in for agent.Transcript, so this package does not
// import internal/agent (which would create an import cycle once the loop is
// wired to this store). The assignment below is the compile-time proof that
// *Session is what the loop's TranscriptStore hands out.
type Transcript interface {
	Key() string
	Messages() []core.Message
	AddMessage(core.Message)
	Clear()
	Save() error
}

// Store.Open returns a concrete *Session, which the agent loop stores behind
// its Transcript interface; this asserts the conversion is possible.
func openTranscript(store *Store, key string) (Transcript, error) {
	return store.Open(key)
}

var _ func(*Store, string) (Transcript, error) = openTranscript

// pythonFixture is the exact byte output of the reference writer
// (manager.py:1314-1341: json.dumps(..., ensure_ascii=False) with default
// separators), generated with CPython 3.14.7 from the same record values.
const pythonFixture = `{"_type": "metadata", "key": "telegram:ç", "created_at": "2026-01-02T03:04:05.123456", "updated_at": "2026-01-02T03:07:44.981233", "metadata": {"last_channel": "telegram:1", "_nanobot_model_preset": "openai", "ratio": 1.0, "count": 7}, "last_archived": 0, "last_consolidated": 0}
{"_type": "provider_state", "state": {"kind": "openai_responses", "provider": "openai:default", "model": "gpt-5", "version": 1, "payload": {"response_id": "resp_abc123"}, "pending_messages": []}}
{"role": "user", "content": "olá — summarize <this> & that", "timestamp": "2026-01-02T03:04:05.124001", "media": ["/home/u/.nanobot/media/telegram/1_photo.png"]}
{"role": "assistant", "content": "", "timestamp": "2026-01-02T03:04:09.551200", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "exec_session", "arguments": "{\"session_id\":\"abc\",\"input\":\"ls\\n\"}"}}], "reasoning_content": "I should list the directory first."}
{"role": "tool", "tool_call_id": "call_1", "name": "exec_session", "content": "file1.txt\nfile2.txt", "timestamp": "2026-01-02T03:04:10.220010"}
{"_type": "future_record_v9", "payload": {"nested": [1, 2.5]}, "timestamp": "2026-01-02T03:04:11.000000"}
{"role": "assistant", "content": "The directory contains two files.", "timestamp": "2026-01-02T03:04:12.004500", "latency_ms": 8123}
`

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore("", t.TempDir())
	if store.Dir() == "" {
		t.Fatal("store has no directory")
	}
	return store
}

func TestStorageKeyMatchesPython(t *testing.T) {
	// Expected stems verified with CPython 3.14.7:
	//   base64.urlsafe_b64encode(key.encode()).decode().rstrip("=")
	cases := []struct{ key, stem string }{
		{"telegram:1", "dGVsZWdyYW06MQ"},
		{"unified:default", "dW5pZmllZDpkZWZhdWx0"},
		{"cli:default", "Y2xpOmRlZmF1bHQ"},
		{"telegram:ç", "dGVsZWdyYW06w6c"},
		{"telegram:-1001234567890", "dGVsZWdyYW06LTEwMDEyMzQ1Njc4OTA"},
		{"telegram:a_b", "dGVsZWdyYW06YV9i"},
		{"telegram:a:b", "dGVsZWdyYW06YTpi"},
		{"dream:20260528-100000", "ZHJlYW06MjAyNjA1MjgtMTAwMDAw"},
		{"discord:guild:123", "ZGlzY29yZDpndWlsZDoxMjM"},
		{"websocket:123e4567-e89b-12d3-a456-426614174000",
			"d2Vic29ja2V0OjEyM2U0NTY3LWU4OWItMTJkMy1hNDU2LTQyNjYxNDE3NDAwMA"},
		{"", ""},
		{"a", "YQ"},
		{"ab", "YWI"},
		{"abc", "YWJj"},
		{"telegram:😀", "dGVsZWdyYW068J-YgA"},
	}
	for _, tc := range cases {
		if got := StorageKey(tc.key); got != tc.stem {
			t.Errorf("StorageKey(%q) = %q, want %q", tc.key, got, tc.stem)
		}
		key, ok := DecodeStorageKey(tc.stem)
		if !ok || key != tc.key {
			t.Errorf("DecodeStorageKey(%q) = (%q, %v), want (%q, true)", tc.stem, key, ok, tc.key)
		}
	}
}

func TestDecodeStorageKeyMatchesPython(t *testing.T) {
	// Expected values produced by the verbatim reference algorithm
	// (manager.py:1009-1017) on CPython 3.14.7. A model of that algorithm was
	// additionally differential-tested against CPython over 581,890 stems.
	cases := []struct {
		stem string
		key  string
		ok   bool
	}{
		{"", "", true},
		{"dGVsZWdyYW06MQ==", "telegram:1", true}, // padded: decodes, but is not canonical
		{"telegram_1", "", false},
		{"YQxx", "a\x0cq", true},
		{"not-base64!!", "", false},
		{"YQ", "a", true},
		{"YWI", "ab", true},
		{"abc", "", false},
		{"a", "", false},
		{"ab", "i", true},
		{"====", "", true},
		{"AA", "\x00", true},
		{"AAA", "\x00\x00", true},
		{"AAAA", "\x00\x00\x00", true},
		{"AAAAA", "", false},
		{"8J-YgA", "😀", true},
		{"é", "", false},
		{"____", "", false}, // decodes to invalid UTF-8
		{"----", "", false}, // decodes to invalid UTF-8
		{"YQ==YQ==", "a\x06\x10", true},
		{"YQ!=", "", false},
		{"=YQ==", "a", true},
		{"0000", "", false}, // decodes to invalid UTF-8
		{"YWJjZA", "abcd", true},
		{"aGk", "hi", true},
	}
	for _, tc := range cases {
		key, ok := DecodeStorageKey(tc.stem)
		if ok != tc.ok || (ok && key != tc.key) {
			t.Errorf("DecodeStorageKey(%q) = (%q, %v), want (%q, %v)", tc.stem, key, ok, tc.key, tc.ok)
		}
	}
}

func TestSessionKeyFromStemRejectsNonCanonical(t *testing.T) {
	cases := []struct {
		stem string
		ok   bool
	}{
		{"dGVsZWdyYW06MQ", true},
		{"dGVsZWdyYW06MQ==", false}, // padding is not canonical
		{"telegram_1", false},       // the retired lossy stem
		{"", true},
	}
	for _, tc := range cases {
		if _, ok := SessionKeyFromStem(tc.stem); ok != tc.ok {
			t.Errorf("SessionKeyFromStem(%q) ok = %v, want %v", tc.stem, ok, tc.ok)
		}
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	store := newTestStore(t)
	sess, err := store.Open("telegram:1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(sess.Messages()) != 0 {
		t.Fatalf("new session has %d messages", len(sess.Messages()))
	}
	sess.Metadata()["last_channel"] = "telegram:1"
	sess.Metadata()["nested"] = map[string]any{"a": 1}
	sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("olá <b> & ç")})
	sess.AddMessage(core.Message{
		Role:    core.RoleAssistant,
		Content: core.TextContent(""),
		ToolCalls: []core.ToolCall{{
			ID:        "call_1",
			Name:      "exec_session",
			Arguments: json.RawMessage(`{"input":"ls"}`),
		}},
	})
	sess.AddMessage(core.Message{
		Role:       core.RoleTool,
		Content:    core.TextContent("ok"),
		Name:       "exec_session",
		ToolCallID: "call_1",
	})
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := store.Open("telegram:1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	msgs := reopened.Messages()
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if got := msgs[0].Content.Text; got != "olá <b> & ç" {
		t.Errorf("content = %q", got)
	}
	if msgs[0].Role != core.RoleUser {
		t.Errorf("role = %q", msgs[0].Role)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "exec_session" {
		t.Fatalf("tool call lost: %+v", msgs[1].ToolCalls)
	}
	if got := string(msgs[1].ToolCalls[0].Arguments); got != `{"input":"ls"}` {
		t.Errorf("tool arguments = %q", got)
	}
	if msgs[2].ToolCallID != "call_1" || msgs[2].Name != "exec_session" {
		t.Errorf("tool row = %+v", msgs[2])
	}
	meta := reopened.Metadata()
	if meta["last_channel"] != "telegram:1" {
		t.Errorf("metadata lost: %+v", meta)
	}
	if _, ok := meta["nested"].(map[string]any); !ok {
		t.Errorf("nested metadata lost: %T", meta["nested"])
	}
	if ts := msgs[0].Timestamp; ts == "" {
		t.Error("timestamp was not stamped by AddMessage")
	}
}

// TestGoWrittenFileUsesPythonSyntax checks the bytes, not just the round trip:
// separators, key order, no HTML escaping, naive timestamps.
func TestGoWrittenFileUsesPythonSyntax(t *testing.T) {
	store := newTestStore(t)
	sess, err := store.Open("cli:default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("a<b>&c")})
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(store.Path("cli:default"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), data)
	}
	if !strings.HasPrefix(lines[0], `{"_type": "metadata", "key": "cli:default", "created_at": "`) {
		t.Errorf("metadata line is not Python-shaped:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], `"last_archived": 0, "last_consolidated": 0}`) {
		t.Errorf("offsets are not integers in Python order:\n%s", lines[0])
	}
	if strings.Contains(string(data), `\u003c`) || strings.Contains(string(data), `\u0026`) {
		t.Errorf("HTML escaping leaked into the file:\n%s", data)
	}
	if !strings.Contains(lines[1], `"content": "a<b>&c"`) {
		t.Errorf("content is not raw UTF-8:\n%s", lines[1])
	}
	// Naive local ISO-8601: no "Z", no offset, six fractional digits or none.
	re := regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d{6})?$`)
	field := regexp.MustCompile(`"updated_at": "([^"]*)"`)
	m := field.FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("no updated_at in %s", lines[0])
	}
	if !re.MatchString(m[1]) {
		t.Errorf("updated_at %q is not a naive local ISO-8601 timestamp", m[1])
	}
	if strings.HasSuffix(m[1], "Z") || strings.ContainsAny(m[1], "+") {
		t.Errorf("updated_at %q carries a timezone", m[1])
	}
}

// TestPythonFixtureRoundTrip loads a file written by the reference and checks
// that nothing the Go model does not understand is lost.
func TestPythonFixtureRoundTrip(t *testing.T) {
	store := newTestStore(t)
	const key = "telegram:ç"
	if err := os.WriteFile(store.Path(key), []byte(pythonFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msgs := sess.Messages()
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5", len(msgs))
	}
	if got := msgs[0].Content.Text; got != "olá — summarize <this> & that" {
		t.Errorf("user content = %q", got)
	}
	// The OpenAI-shaped tool_calls must be rebuilt into core.ToolCall.
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %+v", msgs[1].ToolCalls)
	}
	call := msgs[1].ToolCalls[0]
	if call.ID != "call_1" || call.Name != "exec_session" {
		t.Errorf("tool call = %+v", call)
	}
	if got := string(call.Arguments); got != `{"session_id":"abc","input":"ls\n"}` {
		t.Errorf("tool arguments = %q", got)
	}
	if msgs[1].ReasoningContent != "I should list the directory first." {
		t.Errorf("reasoning = %q", msgs[1].ReasoningContent)
	}
	if msgs[2].ToolCallID != "call_1" || msgs[2].Content.Text != "file1.txt\nfile2.txt" {
		t.Errorf("tool row = %+v", msgs[2])
	}
	// The record with an unknown "_type" is a message, per manager.py:1089-1090.
	if raw, ok := msgs[3].Extra("_type"); !ok || string(raw) != `"future_record_v9"` {
		t.Errorf("unknown record _type = %s (ok=%v)", raw, ok)
	}
	if raw, ok := msgs[3].Extra("payload"); !ok || string(raw) != `{"nested": [1, 2.5]}` {
		t.Errorf("unknown record payload = %s (ok=%v)", raw, ok)
	}
	// Numbers keep Python's int/float distinction.
	ratio, ok := sess.Metadata()["ratio"]
	if !ok {
		t.Fatalf("metadata ratio missing: %+v", sess.Metadata())
	}
	if num, isNum := ratio.(json.Number); !isNum || num.String() != "1.0" {
		t.Errorf("ratio = %#v, want json.Number(\"1.0\")", ratio)
	}
	if count, isNum := sess.Metadata()["count"].(json.Number); !isNum || count.String() != "7" {
		t.Errorf("count = %#v, want json.Number(\"7\")", sess.Metadata()["count"])
	}
	if sess.UpdatedAt().IsZero() {
		t.Error("updated_at was not parsed")
	}
	if state := sess.ProviderState(); len(state) == 0 {
		t.Error("provider_state record was dropped")
	}

	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := os.ReadFile(store.Path(key))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	outLines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	wantLines := strings.Split(strings.TrimSuffix(pythonFixture, "\n"), "\n")
	if len(outLines) != len(wantLines) {
		t.Fatalf("got %d lines, want %d:\n%s", len(outLines), len(wantLines), out)
	}
	// Line 1 is regenerated (it is the only record the writer always rewrites),
	// but every other record must be byte-identical to what Python wrote.
	for i := 1; i < len(wantLines); i++ {
		if outLines[i] != wantLines[i] {
			t.Errorf("line %d changed:\n got %s\nwant %s", i+1, outLines[i], wantLines[i])
		}
	}
	if !strings.Contains(outLines[0], `"metadata": {"last_channel": "telegram:1", "_nanobot_model_preset": "openai", "ratio": 1.0, "count": 7}`) {
		t.Errorf("metadata record was not preserved in Python's order/shape:\n%s", outLines[0])
	}
	if !strings.Contains(outLines[0], `"created_at": "2026-01-02T03:04:05.123456"`) ||
		!strings.Contains(outLines[0], `"updated_at": "2026-01-02T03:07:44.981233"`) {
		t.Errorf("timestamps were not preserved:\n%s", outLines[0])
	}

	// A second load/save cycle must be a fixed point.
	again, err := store.Open(key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := again.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	out2, err := os.ReadFile(store.Path(key))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(out, out2) {
		t.Errorf("save is not idempotent:\n--- first\n%s\n--- second\n%s", out, out2)
	}
}

func TestPythonFixtureLoadedByPythonSemantics(t *testing.T) {
	// The fixture's first line must stay a metadata record with no leading
	// blank line, or read_metadata/update_metadata/list_sessions reject the
	// file (manager.py:1505-1506, :1380-1381, :1552-1556).
	store := newTestStore(t)
	const key = "telegram:ç"
	if err := os.WriteFile(store.Path(key), []byte(pythonFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	keys, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != key {
		t.Errorf("List = %v, want [%q]", keys, key)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), StorageKey(key)+checkpointSuffix)); !os.IsNotExist(err) {
		t.Errorf("unexpected checkpoint sidecar: %v", err)
	}
}

func TestConcurrentSaveDoesNotCorrupt(t *testing.T) {
	store := newTestStore(t)
	sess, err := store.Open("telegram:1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 8; i++ {
		sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("hello")})
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("base Save: %v", err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				// Half the goroutines save the same *Session object.
				sess.AddMessage(core.Message{Role: core.RoleAssistant, Content: core.TextContent("reply")})
				errs[i] = sess.Save()
				return
			}
			// The rest open their own handle for the same key, so the
			// whole-file rewrite really does race across handles.
			s, err := store.Open("telegram:1")
			if err != nil {
				errs[i] = err
				return
			}
			s.AddMessage(core.Message{Role: core.RoleAssistant, Content: core.TextContent("reply")})
			errs[i] = s.Save()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(store.Path("telegram:1"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON (%v): %s", i+1, err, line)
		}
	}
	if len(lines) < 2 {
		t.Fatalf("file lost its records:\n%s", data)
	}
	if !strings.HasPrefix(lines[0], `{"_type": "metadata"`) {
		t.Errorf("first line is not the metadata record: %s", lines[0])
	}
	// Every save wrote a complete snapshot, so the base transcript must still
	// be intact and at least one assistant row must have survived.
	reopened, err := store.Open("telegram:1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	msgs := reopened.Messages()
	users, assistants := 0, 0
	for _, m := range msgs {
		switch m.Role {
		case core.RoleUser:
			if m.Content.Text != "hello" {
				t.Errorf("user row was corrupted: %q", m.Content.Text)
			}
			users++
		case core.RoleAssistant:
			if m.Content.Text != "reply" {
				t.Errorf("assistant row was corrupted: %q", m.Content.Text)
			}
			assistants++
		}
	}
	if users != 8 {
		t.Errorf("got %d user rows, want 8 (a save lost messages)", users)
	}
	if assistants < 1 {
		t.Errorf("got %d assistant rows, want at least 1", assistants)
	}
	// No temp files may be left behind.
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestListAndDelete(t *testing.T) {
	store := newTestStore(t)
	for _, key := range []string{"telegram:1", "telegram:2", "cli:default"} {
		sess, err := store.Open(key)
		if err != nil {
			t.Fatalf("Open(%q): %v", key, err)
		}
		sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent(key)})
		if err := sess.Save(); err != nil {
			t.Fatalf("Save(%q): %v", key, err)
		}
	}
	// A non-canonical stem must be invisible to List, like the reference.
	lossy := filepath.Join(store.Dir(), "telegram_1.jsonl")
	if err := os.WriteFile(lossy, []byte(pythonFixture), 0o644); err != nil {
		t.Fatalf("write lossy: %v", err)
	}
	keys, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("List = %v, want 3 keys", keys)
	}
	for _, want := range []string{"telegram:1", "telegram:2", "cli:default"} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("List is missing %q: %v", want, keys)
		}
	}

	if err := store.Delete("telegram:1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(store.Path("telegram:1")); !os.IsNotExist(err) {
		t.Errorf("session file still present: %v", err)
	}
	if _, err := os.Stat(lossy); !os.IsNotExist(err) {
		t.Errorf("retired lossy path still present: %v", err)
	}
	keys, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("List after Delete = %v", keys)
	}
	// Deleting an absent key is not an error.
	if err := store.Delete("telegram:99"); err != nil {
		t.Errorf("Delete(absent) = %v", err)
	}
}

func TestClearResetsTranscript(t *testing.T) {
	store := newTestStore(t)
	sess, err := store.Open("cli:default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess.Metadata()["_last_summary"] = map[string]any{"text": "old"}
	sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("hi")})
	sess.SetLastArchived(1)
	sess.Clear()
	if len(sess.Messages()) != 0 {
		t.Errorf("Clear left %d messages", len(sess.Messages()))
	}
	if sess.LastArchived() != 0 {
		t.Errorf("Clear left last_archived = %d", sess.LastArchived())
	}
	if _, ok := sess.Metadata()["_last_summary"]; ok {
		t.Error("Clear did not drop _last_summary")
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := store.Open("cli:default")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(reopened.Messages()) != 0 {
		t.Errorf("cleared transcript came back with %d messages", len(reopened.Messages()))
	}
}

func TestStoreRejectsSessionsRootInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace, filepath.Join(workspace, "sessions"))
	if _, err := store.Open("cli:default"); err == nil {
		t.Fatal("expected an error for a sessions root inside the workspace")
	} else if !strings.Contains(err.Error(), "outside the agent workspace") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStoreNamespacesByWorkspaceID(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	store := NewStore(workspace, root)
	if _, err := store.Open("cli:default"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, err := os.ReadFile(filepath.Join(workspace, ".nanobot", "workspace-id"))
	if err != nil {
		t.Fatalf("read workspace-id: %v", err)
	}
	value := strings.TrimSpace(string(id))
	if !workspaceIDRe.MatchString(value) {
		t.Fatalf("workspace-id = %q, want 32 lowercase hex characters", value)
	}
	if filepath.Base(store.Dir()) != value {
		t.Errorf("Dir() = %s, want it to end in %s", store.Dir(), value)
	}
	marker, err := os.ReadFile(filepath.Join(store.Dir(), ".workspace"))
	if err != nil {
		t.Fatalf("read .workspace: %v", err)
	}
	if strings.TrimSpace(string(marker)) == "" {
		t.Error(".workspace marker is empty")
	}
	// A second store on the same workspace must reuse the same namespace.
	other := NewStore(workspace, root)
	if other.Dir() != store.Dir() {
		t.Errorf("second store namespace = %s, want %s", other.Dir(), store.Dir())
	}
}

func TestCheckpointOverlay(t *testing.T) {
	store := newTestStore(t)
	const key = "cli:default"
	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("hi")})
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := store.Path(key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fields, err := objectFields(bytes.SplitN(data, []byte("\n"), 2)[0])
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	updated, _ := stringField(fields, "updated_at")

	// A checkpoint whose base_updated_at and base_message_count match must be
	// overlaid into metadata (manager.py:1279-1299).
	payload := `{"version": 1, "session_key": "` + key + `", "base_updated_at": "` + updated +
		`", "base_message_count": 1, "checkpoint": {"phase": "final_response"}, "provider_state": null}`
	cp := store.checkpointPath(key)
	if err := os.WriteFile(cp, []byte(payload), 0o600); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	// The sidecar must not look older than the JSONL, or it is treated as
	// stale and unlinked (manager.py:1275-1277).
	if err := os.Chtimes(cp, nowTimestamp(), nowTimestamp()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	overlaid, err := store.Open(key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := overlaid.Metadata()["runtime_checkpoint"]; !ok {
		t.Errorf("checkpoint was not overlaid: %+v", overlaid.Metadata())
	}

	// A mismatching base_updated_at must discard the sidecar.
	bad := `{"version": 1, "session_key": "` + key + `", "base_updated_at": "1999-01-01T00:00:00", "base_message_count": 1, "checkpoint": {}, "provider_state": null}`
	if err := os.WriteFile(cp, []byte(bad), 0o600); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	if err := os.Chtimes(cp, nowTimestamp(), nowTimestamp()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	discarded, err := store.Open(key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := discarded.Metadata()["runtime_checkpoint"]; ok {
		t.Error("stale checkpoint was applied")
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Errorf("invalid checkpoint was not unlinked: %v", err)
	}
}

func TestSaveRemovesCheckpointSidecar(t *testing.T) {
	store := newTestStore(t)
	const key = "cli:default"
	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("hi")})
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cp := store.checkpointPath(key)
	if err := os.WriteFile(cp, []byte(`{"version": 1}`), 0o600); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Errorf("a full save must unlink the checkpoint sidecar (manager.py:1348): %v", err)
	}
}

func TestOpenTolerantOfCorruptLines(t *testing.T) {
	store := newTestStore(t)
	const key = "cli:default"
	content := `{"_type": "metadata", "key": "cli:default", "created_at": "2026-01-02T03:04:05", "updated_at": "2026-01-02T03:04:06", "metadata": {}, "last_archived": 0, "last_consolidated": 0}
this is not json
[1, 2, 3]
{"role": "user", "content": "survives", "timestamp": "2026-01-02T03:04:07"}
`
	if err := os.WriteFile(store.Path(key), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msgs := sess.Messages()
	if len(msgs) != 1 || msgs[0].Content.Text != "survives" {
		t.Fatalf("messages = %+v", msgs)
	}
	// repair() never writes, so the corrupt lines must still be on disk.
	data, err := os.ReadFile(store.Path(key))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "this is not json") {
		t.Error("Open rewrote the file")
	}
}

func TestTimestampNormalisationMatchesPython(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026-01-02T03:04:05.1", "2026-01-02T03:04:05.100000"},
		{"2026-01-02T03:04:05.000000", "2026-01-02T03:04:05"},
		{"2026-01-02 03:04:05", "2026-01-02T03:04:05"},
		{"2026-01-02T03:04:05.123456789", "2026-01-02T03:04:05.123456"},
		{"2026-01-02", "2026-01-02T00:00:00"},
	}
	for _, tc := range cases {
		_, normalized, ok := parseTimestamp(tc.in)
		if !ok {
			t.Errorf("parseTimestamp(%q) failed", tc.in)
			continue
		}
		if normalized != tc.want {
			t.Errorf("parseTimestamp(%q) = %q, want %q", tc.in, normalized, tc.want)
		}
	}
	if _, _, ok := parseTimestamp("not a timestamp"); ok {
		t.Error("parseTimestamp accepted a non-timestamp")
	}
}

func TestPythonJSONEncoding(t *testing.T) {
	// Expected strings verified with CPython 3.14.7 json.dumps(..., ensure_ascii=False).
	cases := []struct{ in, want string }{
		{"a<b>&c", `"a<b>&c"`},
		{"\u2028\u2029", "\"\u2028\u2029\""},
		{"é😀", "\"é😀\""},
		{"/", `"/"`},
		{"\x00\x0b", `"\u0000\u000b"`},
		{"\x7f", "\"\x7f\""},
		{"a\n\t\r\b\f\"\\", `"a\n\t\r\b\f\"\\"`},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		writePyString(&buf, tc.in)
		if buf.String() != tc.want {
			t.Errorf("writePyString(%q) = %s, want %s", tc.in, buf.String(), tc.want)
		}
	}

	floats := []struct {
		in   float64
		want string
	}{
		{1.0, "1.0"},
		{1e20, "1e+20"},
		{1e16, "1e+16"},
		{1e15, "1000000000000000.0"},
		{0.1, "0.1"},
		{0.0001, "0.0001"},
		{0.00001, "1e-05"},
		{1.5e-07, "1.5e-07"},
	}
	for _, tc := range floats {
		var buf bytes.Buffer
		writePyFloat(&buf, tc.in)
		if buf.String() != tc.want {
			t.Errorf("writePyFloat(%v) = %s, want %s", tc.in, buf.String(), tc.want)
		}
	}

	// A json.Number passes through verbatim, which is what keeps a Python
	// float from becoming a Go int.
	var buf bytes.Buffer
	if err := appendPyValue(&buf, json.Number("1.0")); err != nil {
		t.Fatalf("appendPyValue: %v", err)
	}
	if buf.String() != "1.0" {
		t.Errorf("json.Number(1.0) = %s", buf.String())
	}
	buf.Reset()
	if err := appendPyRaw(&buf, json.RawMessage(`{"b": 1, "a": [1.0, 2]}`)); err != nil {
		t.Fatalf("appendPyRaw: %v", err)
	}
	if buf.String() != `{"b": 1, "a": [1.0, 2]}` {
		t.Errorf("appendPyRaw = %s", buf.String())
	}
}

func TestFloatMetadataSurvivesGoRoundTrip(t *testing.T) {
	store := newTestStore(t)
	const key = "telegram:ç"
	if err := os.WriteFile(store.Path(key), []byte(pythonFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(store.Path(key))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"ratio": 1.0`) {
		t.Errorf("a Python float became an int on the Go round trip:\n%s", data)
	}
}

// TestExtrasAreReEncodedWithoutHTMLEscaping covers the SetEscapeHTML trap: a
// caller that produced its raw value with encoding/json (which escapes
// '<', '>', '&') must not leak those escapes into the session file, because
// Python stores them raw and compares digests byte for byte.
func TestExtrasAreReEncodedWithoutHTMLEscaping(t *testing.T) {
	store := newTestStore(t)
	sess, err := store.Open("cli:default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m := core.Message{Role: core.RoleUser, Content: core.TextContent("plain")}
	escaped, err := json.Marshal("a<b>&c\u2028d")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(escaped), `\u003c`) {
		t.Fatalf("test precondition failed: %s", escaped)
	}
	m.SetExtra("note", escaped)
	m.SetExtra("latency_ms", json.RawMessage(`1234`))
	sess.AddMessage(m)
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(store.Path("cli:default"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"note": "a<b>&c`+"\u2028"+`d"`) {
		t.Errorf("extras kept Go's HTML escaping:\n%s", data)
	}
	if !strings.Contains(string(data), `"latency_ms": 1234`) {
		t.Errorf("integer extra was not preserved:\n%s", data)
	}
}

// TestSessionKeyCannotEscapeDirectory checks that a hostile key cannot make
// the store read or delete anything outside its directory. StorageKey is
// base64url, so it never contains a separator or a dot.
func TestSessionKeyCannotEscapeDirectory(t *testing.T) {
	root := t.TempDir()
	store := NewStore("", root)
	outside := filepath.Join(filepath.Dir(root), "victim.jsonl")
	if err := os.WriteFile(outside, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	defer os.Remove(outside)

	for _, key := range []string{"../../etc/passwd", "..", "/etc/passwd", "a/b/c"} {
		path := store.Path(key)
		if filepath.Dir(path) != root {
			t.Errorf("Path(%q) = %s, escaped %s", key, path, root)
		}
		if _, err := store.Open(key); err != nil {
			t.Errorf("Open(%q): %v", key, err)
		}
		if err := store.Delete(key); err != nil {
			t.Errorf("Delete(%q): %v", key, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep me" {
		t.Errorf("file outside the store was touched: %q, %v", data, err)
	}
}

// TestMultimodalContentAndHiddenMarkersPreserved checks a Python-written
// content array, a hidden-history marker and a runtime-context marker survive
// a Go load/save byte for byte.
func TestMultimodalContentAndHiddenMarkersPreserved(t *testing.T) {
	const key = "cli:default"
	fixture := `{"_type": "metadata", "key": "cli:default", "created_at": "2026-01-02T03:04:05", "updated_at": "2026-01-02T03:04:06", "metadata": {}, "last_archived": 0, "last_consolidated": 0}
{"role": "user", "content": [{"type": "text", "text": "look"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}, "_meta": {"path": "/tmp/a.png"}}], "timestamp": "2026-01-02T03:04:07.000001"}
{"role": "user", "content": "Continue the active task from the working-memory checkpoint above.", "timestamp": "2026-01-02T03:04:08", "_hidden_history": true}
{"role": "user", "content": "what did I ask?\n\n[Runtime Context]", "timestamp": "2026-01-02T03:04:09", "_runtime_context": {"version": 1, "sources": ["goal_state"], "suffix": "[Runtime Context]"}}
`
	store := newTestStore(t)
	if err := os.WriteFile(store.Path(key), []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msgs := sess.Messages()
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if !msgs[0].Content.IsText() && len(msgs[0].Content.Blocks) != 2 {
		t.Errorf("content blocks = %+v", msgs[0].Content)
	}
	if !msgs[1].IsHiddenHistory() {
		t.Error("_hidden_history was not visible through core.Message")
	}
	if _, ok := msgs[2].Extra("_runtime_context"); !ok {
		t.Error("_runtime_context marker was lost")
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := os.ReadFile(store.Path(key))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	wantLines := strings.Split(strings.TrimSuffix(fixture, "\n"), "\n")
	gotLines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("got %d lines, want %d:\n%s", len(gotLines), len(wantLines), out)
	}
	for i := 1; i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			t.Errorf("line %d changed:\n got %s\nwant %s", i+1, gotLines[i], wantLines[i])
		}
	}
}
