package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

// These tests prove real interoperability of the on-disk session format: the
// Python reference writes a session and nanobot-go reads it, then nanobot-go
// writes one and the Python reference reads it.
//
// This is the concrete verification of the "same ~/.nanobot/" requirement. It
// is deliberately NOT a byte-comparison: both implementations parse JSON into a
// mapping, so key order is not significant. What must match is the decoded
// structure, the storage filename, and the workspace namespace.

const interopKey = "cli:interop"

// runInterop runs the Python harness and returns its parsed stdout.
func runInterop(t *testing.T, args ...string) map[string]any {
	t.Helper()
	root := repoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "interop_session.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — interop check not run", python)
	}

	cmd := exec.Command(python, append([]string{script}, args...)...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("interop harness %v failed: %v\n%s", args, err, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse harness output: %v\n%s", err, out)
	}
	return doc
}

func interopPaths(t *testing.T) (workspace, sessionsRoot string) {
	t.Helper()
	base := filepath.Join(repoRoot(t), ".tools", "interop")
	return filepath.Join(base, "ws"), filepath.Join(base, "sessions")
}

// TestGoReadsPythonSession is the primary direction: an existing ~/.nanobot/
// written by the Python implementation must load correctly in Go.
func TestGoReadsPythonSession(t *testing.T) {
	workspace, sessionsRoot := interopPaths(t)
	if err := os.RemoveAll(filepath.Join(repoRoot(t), ".tools", "interop")); err != nil {
		t.Fatalf("clean interop dir: %v", err)
	}

	written := runInterop(t, "write", workspace, sessionsRoot)
	wantCount := int(written["message_count"].(float64))

	store := session.NewStore(workspace, sessionsRoot)
	sess, err := store.Open(interopKey)
	if err != nil {
		t.Fatalf("Go failed to open the Python-written session: %v", err)
	}
	msgs := sess.Messages()

	// The Python metadata record is not a message, so counts must agree.
	if len(msgs) != wantCount {
		t.Fatalf("message count = %d, Python wrote %d", len(msgs), wantCount)
	}

	// 1. The Go store must have landed in the SAME namespace directory the
	//    Python store created. A different workspace-id would mean the two
	//    implementations silently keep separate histories.
	pyPath := written["path"].(string)
	pyDir := filepath.Base(filepath.Dir(pyPath))
	if got := filepath.Base(store.Dir()); got != pyDir {
		t.Errorf("workspace namespace mismatch: python=%s go=%s", pyDir, got)
	}
	// And it must resolve to the identical file.
	if got, want := store.Path(interopKey), pyPath; got != want {
		t.Errorf("session path mismatch:\n  go     = %s\n  python = %s", got, want)
	}

	// 2. Decoded content must match exactly, including unicode and the
	//    characters Go's encoder escapes by default.
	want := []struct {
		role    core.Role
		content string
	}{
		{core.RoleUser, "hello world"},
		{core.RoleAssistant, "hi there"},
		{core.RoleUser, "acentuação, 日本語, emoji 🎉"},
		{core.RoleAssistant, ""},
		{core.RoleTool, "file contents"},
		{core.RoleUser, "a < b & c > d"},
	}
	for i, w := range want {
		if msgs[i].Role != w.role {
			t.Errorf("msg[%d].role = %q, want %q", i, msgs[i].Role, w.role)
		}
		if msgs[i].Content.Text != w.content {
			t.Errorf("msg[%d].content = %q, want %q", i, msgs[i].Content.Text, w.content)
		}
	}

	// 3. The tool call must survive with its id, name and arguments.
	call := msgs[3].ToolCalls
	if len(call) != 1 {
		t.Fatalf("msg[3] tool_calls = %d, want 1", len(call))
	}
	if call[0].ID != "call_1" {
		t.Errorf("tool call id = %q, want %q", call[0].ID, "call_1")
	}
	if call[0].Name != "read_file" {
		t.Errorf("tool call name = %q, want %q", call[0].Name, "read_file")
	}
	var args map[string]any
	if err := json.Unmarshal(call[0].Arguments, &args); err != nil {
		t.Fatalf("tool call arguments not valid JSON: %v (%s)", err, call[0].Arguments)
	}
	if args["path"] != "a.txt" {
		t.Errorf("tool call argument path = %v, want a.txt", args["path"])
	}

	// 4. The tool result must keep its correlation id — without it the next
	//    request to the model is invalid.
	if msgs[4].ToolCallID != "call_1" {
		t.Errorf("tool message tool_call_id = %q, want %q", msgs[4].ToolCallID, "call_1")
	}
	if msgs[4].Name != "read_file" {
		t.Errorf("tool message name = %q, want %q", msgs[4].Name, "read_file")
	}

	// 5. Timestamps must be preserved verbatim (naive local ISO).
	for i, m := range msgs {
		if m.Timestamp == "" {
			t.Errorf("msg[%d] lost its timestamp", i)
		}
	}
}

// TestPythonReadsGoSession is the reverse direction: a session written by
// nanobot-go must be loadable by the Python implementation. Without this, the
// port could write files that only it understands.
func TestPythonReadsGoSession(t *testing.T) {
	workspace, sessionsRoot := interopPaths(t)
	if err := os.RemoveAll(filepath.Join(repoRoot(t), ".tools", "interop")); err != nil {
		t.Fatalf("clean interop dir: %v", err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		t.Fatalf("mkdir sessions root: %v", err)
	}

	// Create the store first so the workspace-id namespace exists exactly as
	// the reference would create it.
	store := session.NewStore(workspace, sessionsRoot)
	sess, err := store.Open(interopKey)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	user := core.NewMessage(core.RoleUser, "written by go")
	sess.AddMessage(*user)

	assistant := core.NewMessage(core.RoleAssistant, "")
	assistant.ToolCalls = []core.ToolCall{{
		ID:        "call_go_1",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"b.txt"}`),
	}}
	sess.AddMessage(*assistant)

	tool := core.NewMessage(core.RoleTool, "go tool output")
	tool.ToolCallID = "call_go_1"
	tool.Name = "read_file"
	sess.AddMessage(*tool)

	sess.AddMessage(*core.NewMessage(core.RoleUser, "unicode ✓ 日本 <&>"))

	if err := sess.Save(); err != nil {
		t.Fatalf("save session: %v", err)
	}

	// Now let the Python reference read it back.
	read := runInterop(t, "read", workspace, sessionsRoot, interopKey)
	if errStr, ok := read["error"].(string); ok {
		t.Fatalf("Python could not read the Go-written session: %s", errStr)
	}

	gotCount := int(read["message_count"].(float64))
	if gotCount != 4 {
		t.Fatalf("Python read %d messages, Go wrote 4", gotCount)
	}

	msgs := read["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "written by go" {
		t.Errorf("python read msg[0] = %v", first)
	}

	// The tool result's correlation id must have survived the Go writer.
	third := msgs[2].(map[string]any)
	if third["role"] != "tool" {
		t.Errorf("python read msg[2].role = %v, want tool", third["role"])
	}
	if third["tool_call_id"] != "call_go_1" {
		t.Errorf("python read msg[2].tool_call_id = %v, want call_go_1", third["tool_call_id"])
	}
	if third["name"] != "read_file" {
		t.Errorf("python read msg[2].name = %v, want read_file", third["name"])
	}

	// Unicode and the characters Go escapes by default must round-trip.
	fourth := msgs[3].(map[string]any)
	if fourth["content"] != "unicode ✓ 日本 <&>" {
		t.Errorf("python read msg[3].content = %v", fourth["content"])
	}

	// The assistant tool call must be intact too.
	second := msgs[1].(map[string]any)
	calls, ok := second["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("python read msg[1].tool_calls = %v", second["tool_calls"])
	}
	call0 := calls[0].(map[string]any)
	if call0["id"] != "call_go_1" {
		t.Errorf("python read tool call id = %v, want call_go_1", call0["id"])
	}
}
