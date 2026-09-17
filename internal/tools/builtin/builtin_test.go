package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func execTool(t *testing.T, tool tools.Tool, raw string) tools.Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("%s.Execute(%s) returned unexpected error: %v", tool.Name(), raw, err)
	}
	return res
}

func mustOK(t *testing.T, tool tools.Tool, raw string) tools.Result {
	t.Helper()
	res := execTool(t, tool, raw)
	if res.IsError {
		t.Fatalf("%s.Execute(%s) = error result %q, want success", tool.Name(), raw, res.Content)
	}
	return res
}

func mustErr(t *testing.T, tool tools.Tool, raw string) tools.Result {
	t.Helper()
	res := execTool(t, tool, raw)
	if !res.IsError {
		t.Fatalf("%s.Execute(%s) = %q, want an error result", tool.Name(), raw, res.Content)
	}
	return res
}

func argsJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Parameters() schema contract
// ---------------------------------------------------------------------------

// TestParametersMatchReference asserts every tool advertises a valid strict
// JSON Schema object whose property names and required list match the reference
// (filesystem.py:251-269, :533-539, :831-858, :1087-1096; shell.py:118-160).
func TestParametersMatchReference(t *testing.T) {
	policy := PathPolicy{}
	cases := []struct {
		tool     tools.Tool
		name     string
		props    []string
		required []string
	}{
		{
			tool:     NewApplyPatch(policy),
			name:     "apply_patch",
			props:    []string{"edits", "dry_run"},
			required: []string{"edits"},
		},
		{
			tool:     NewReadFile(policy),
			name:     "read_file",
			props:    []string{"path", "offset", "limit", "pages", "force"},
			required: []string{"path"},
		},
		{
			tool:     NewWriteFile(policy),
			name:     "write_file",
			props:    []string{"path", "content"},
			required: []string{"path", "content"},
		},
		{
			tool:     NewEditFile(policy),
			name:     "edit_file",
			props:    []string{"path", "old_text", "new_text", "replace_all", "occurrence", "line_hint", "expected_replacements"},
			required: []string{"path", "old_text", "new_text"},
		},
		{
			tool:     NewListDir(policy),
			name:     "list_dir",
			props:    []string{"path", "recursive", "max_entries"},
			required: []string{"path"},
		},
		{
			tool: NewExec(ExecOptions{}),
			name: "exec",
			props: []string{
				"command", "cmd", "working_dir", "workdir", "timeout", "shell", "login",
				"yield_time_ms", "max_output_chars", "max_output_tokens",
			},
			required: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tool.Name(); got != tc.name {
				t.Fatalf("Name() = %q, want %q", got, tc.name)
			}
			if tc.tool.Description() == "" {
				t.Fatal("Description() is empty")
			}
			raw := tc.tool.Parameters()
			if !json.Valid(raw) {
				t.Fatalf("Parameters() is not valid JSON: %s", raw)
			}
			var schema struct {
				Type                 string                     `json:"type"`
				Properties           map[string]json.RawMessage `json:"properties"`
				Required             []string                   `json:"required"`
				AdditionalProperties *bool                      `json:"additionalProperties"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatalf("unmarshal Parameters(): %v", err)
			}
			if schema.Type != "object" {
				t.Fatalf("schema root type = %q, want object", schema.Type)
			}
			if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
				t.Fatalf("schema additionalProperties = %v, want false", schema.AdditionalProperties)
			}
			if len(schema.Properties) != len(tc.props) {
				t.Fatalf("properties = %v, want %v", keysOf(schema.Properties), tc.props)
			}
			for _, want := range tc.props {
				if _, ok := schema.Properties[want]; !ok {
					t.Fatalf("missing property %q in %v", want, keysOf(schema.Properties))
				}
			}
			if strings.Join(schema.Required, ",") != strings.Join(tc.required, ",") {
				t.Fatalf("required = %v, want %v", schema.Required, tc.required)
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestConcurrencyFlags pins the read-only / exclusive classification the runner
// relies on (internal/tools/tool.go:56-109).
func TestConcurrencyFlags(t *testing.T) {
	policy := PathPolicy{}
	readOnly := []tools.Tool{NewReadFile(policy), NewListDir(policy)}
	for _, tool := range readOnly {
		if !tools.IsReadOnly(tool) || !tools.IsConcurrencySafe(tool) || tools.IsExclusive(tool) {
			t.Fatalf("%s: want read-only + concurrency-safe + not exclusive", tool.Name())
		}
	}
	mutating := []tools.Tool{NewWriteFile(policy), NewEditFile(policy), NewApplyPatch(policy)}
	for _, tool := range mutating {
		if tools.IsReadOnly(tool) || tools.IsConcurrencySafe(tool) {
			t.Fatalf("%s: want mutating, not concurrency-safe", tool.Name())
		}
	}
	if !tools.IsExclusive(NewExec(ExecOptions{})) {
		t.Fatal("exec: want exclusive")
	}
}

func TestRegister(t *testing.T) {
	reg := tools.NewRegistry()
	Register(reg, Config{})
	// Config{} leaves EnforceCapabilities false, which is the compatibility mode:
	// the historical COMPLETE catalog is registered regardless of the Enable*
	// switches. a2a_call is part of that catalog (it is a built-in tool like any
	// other), so it belongs in this list — the expectation used to stop at
	// python_exec and failed for that reason.
	want := []string{"apply_patch", "read_file", "write_file", "edit_file", "list_dir", "exec", "python_exec", "a2a_call"}
	if got := reg.Names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry names = %v, want %v", got, want)
	}
	for _, name := range want {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("registry missing %q", name)
		}
	}
}

// TestRegisterEnforcesCapabilities is the regression test for the capability
// boundary: with EnforceCapabilities set — which every production composition
// root does — the Enable* switches are authoritative. Before this was wired, a
// deployment could set tools.exec.enable=false and still get a working shell,
// which is worse than offering no switch at all.
func TestRegisterEnforcesCapabilities(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "nothing enabled",
			cfg:  Config{EnforceCapabilities: true},
			want: []string{},
		},
		{
			name: "files only",
			cfg:  Config{EnforceCapabilities: true, EnableFiles: true},
			want: []string{"apply_patch", "read_file", "write_file", "edit_file", "list_dir"},
		},
		{
			name: "exec only",
			cfg:  Config{EnforceCapabilities: true, EnableExec: true},
			// python_exec is arbitrary process execution and must follow the
			// exec switch rather than bypass it.
			want: []string{"exec", "python_exec"},
		},
		{
			name: "network only",
			cfg:  Config{EnforceCapabilities: true, EnableNetwork: true},
			want: []string{"a2a_call"},
		},
		{
			name: "all enabled",
			cfg: Config{
				EnforceCapabilities: true,
				EnableFiles:         true,
				EnableExec:          true,
				EnableNetwork:       true,
			},
			want: []string{"apply_patch", "read_file", "write_file", "edit_file", "list_dir", "exec", "python_exec", "a2a_call"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := tools.NewRegistry()
			Register(reg, tc.cfg)
			if got := reg.Names(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("registry names = %v, want %v", got, tc.want)
			}
			// Tools() must agree with Register: a divergence between the two is
			// how a disabled tool reappears on a different code path.
			fromTools := make([]string, 0, len(tc.want))
			for _, tool := range Tools(tc.cfg) {
				fromTools = append(fromTools, tool.Name())
			}
			if strings.Join(fromTools, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("Tools() names = %v, want %v", fromTools, tc.want)
			}
		})
	}
}

// TestRegistryDispatch exercises the tools through the frozen dispatch path so
// the argument normalization and error-as-content contract is covered end to
// end (internal/tools/tool.go:197-240).
func TestRegistryDispatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hi\n")

	reg := tools.NewRegistry()
	Register(reg, Config{Files: PathPolicy{Workspace: dir}})

	res := reg.Execute(context.Background(), core.ToolCall{
		ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`),
	})
	if res.IsError || !strings.Contains(res.Content, "1| hi") {
		t.Fatalf("read_file via registry = %+v", res)
	}

	// Absent arguments normalize to {} and produce a model-visible error.
	res = reg.Execute(context.Background(), core.ToolCall{ID: "c2", Name: "read_file"})
	if !res.IsError || !strings.Contains(res.Content, "missing required path") {
		t.Fatalf("empty args via registry = %+v", res)
	}

	res = reg.Execute(context.Background(), core.ToolCall{
		ID: "c3", Name: "read_file", Arguments: json.RawMessage(`{"path":"missing.txt"}`),
	})
	if !res.IsError || !strings.Contains(res.Content, "File not found") {
		t.Fatalf("missing file via registry = %+v", res)
	}

	res = reg.Execute(context.Background(), core.ToolCall{ID: "c4", Name: "no_such_tool"})
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Fatalf("unknown tool via registry = %+v", res)
	}
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

func TestReadFileHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "alpha\nbeta\ngamma\n")

	res := mustOK(t, NewReadFile(PathPolicy{}), argsJSON(t, map[string]any{"path": path}))
	want := "1| alpha\n2| beta\n3| gamma\n\n(End of file — 3 lines total)"
	if res.Content != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}
}

func TestReadFileOffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "l1\nl2\nl3\nl4\nl5\n")

	tool := NewReadFile(PathPolicy{})

	res := mustOK(t, tool, argsJSON(t, map[string]any{"path": path, "offset": 2, "limit": 2}))
	want := "2| l2\n3| l3\n\n(Showing lines 2-3 of 5. Use offset=4 to continue.)"
	if res.Content != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}

	// offset below the schema minimum is clamped to 1, mirroring
	// filesystem.py:383-384.
	res = mustOK(t, tool, argsJSON(t, map[string]any{"path": path, "offset": 0, "limit": 1}))
	if !strings.HasPrefix(res.Content, "1| l1") {
		t.Fatalf("clamped offset content = %q", res.Content)
	}

	// offset past EOF is an error the model can observe (filesystem.py:385-386).
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": path, "offset": 99}))
	if !strings.Contains(res.Content, "beyond end of file") {
		t.Fatalf("offset error = %q", res.Content)
	}
}

func TestReadFileAdversarial(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.txt")
	tool := NewReadFile(PathPolicy{})

	res := mustErr(t, tool, argsJSON(t, map[string]any{"path": missing}))
	if !strings.Contains(res.Content, "File not found") {
		t.Fatalf("missing file content = %q", res.Content)
	}

	// A directory is not a file (filesystem.py:318-319).
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": dir}))
	if !strings.Contains(res.Content, "Not a file") {
		t.Fatalf("directory content = %q", res.Content)
	}

	// Device paths that could hang or stream forever are blocked
	// (filesystem.py:210-231, :307-309).
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": "/dev/zero"}))
	if !strings.Contains(res.Content, "blocked") {
		t.Fatalf("device content = %q", res.Content)
	}

	// Binary content with a non-text extension is refused (filesystem.py:369-372).
	bin := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(bin, []byte{0x00, 0x01, 0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": bin}))
	if !strings.Contains(res.Content, "Cannot read binary file") {
		t.Fatalf("binary content = %q", res.Content)
	}

	// Unknown parameters are rejected rather than silently ignored.
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": missing, "bogus": 1}))
	if !strings.Contains(res.Content, "unexpected parameter bogus") {
		t.Fatalf("unknown param content = %q", res.Content)
	}

	// A non-string path is a parameter error, not a panic.
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": map[string]any{"a": 1}}))
	if !strings.Contains(res.Content, "path should be string") {
		t.Fatalf("bad path type content = %q", res.Content)
	}

	// A missing required field is reported.
	res = mustErr(t, tool, `{}`)
	if !strings.Contains(res.Content, "missing required path") {
		t.Fatalf("missing required content = %q", res.Content)
	}

	// Empty file.
	empty := filepath.Join(dir, "empty.txt")
	writeFile(t, empty, "")
	res = mustOK(t, tool, argsJSON(t, map[string]any{"path": empty}))
	if !strings.Contains(res.Content, "(Empty file:") {
		t.Fatalf("empty file content = %q", res.Content)
	}

	// Non-UTF-8 content in a known text extension falls back to latin-1
	// (filesystem.py:352-359).
	latin := filepath.Join(dir, "latin.txt")
	if err := os.WriteFile(latin, []byte{0xe9, '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	res = mustOK(t, tool, argsJSON(t, map[string]any{"path": latin}))
	if !strings.Contains(res.Content, "1| é") {
		t.Fatalf("latin-1 fallback content = %q", res.Content)
	}

	// An image is reported with its magic-byte MIME. The reference would return
	// image content blocks (filesystem.py:342-344); this port is text-only, so
	// the model gets an explicit error instead.
	png := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(png, append([]byte("\x89PNG\r\n\x1a\n"), 0x00, 0x01), 0o644); err != nil {
		t.Fatal(err)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": png}))
	if !strings.Contains(res.Content, "MIME: image/png") {
		t.Fatalf("image content = %q", res.Content)
	}
	if !strings.Contains(res.Content, "not implemented by this port") {
		t.Fatalf("image port note missing: %q", res.Content)
	}
}

// TestReadFileWorkspaceBoundary asserts a path outside the allowed root is
// refused with the reference's boundary note (workspace_policy.py:12-23).
func TestReadFileWorkspaceBoundary(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, outside, "secret\n")

	tool := NewReadFile(PathPolicy{Workspace: root, AllowedDir: root})

	res := mustErr(t, tool, argsJSON(t, map[string]any{"path": outside}))
	if !strings.Contains(res.Content, "outside allowed directory") {
		t.Fatalf("boundary content = %q", res.Content)
	}
	if !strings.Contains(res.Content, "hard policy boundary") {
		t.Fatalf("boundary note missing: %q", res.Content)
	}

	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": "../escape.txt"}))
	if !strings.Contains(res.Content, "outside allowed directory") {
		t.Fatalf("traversal content = %q", res.Content)
	}

	// A path inside the workspace still works.
	inside := filepath.Join(root, "ok.txt")
	writeFile(t, inside, "ok\n")
	mustOK(t, tool, argsJSON(t, map[string]any{"path": "ok.txt"}))
}

// TestReadFileCharCap asserts the 128k character cap (filesystem.py:274, :393-402).
func TestReadFileCharCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	line := strings.Repeat("x", 1000)
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(line)
		b.WriteString("\n")
	}
	writeFile(t, path, b.String())

	res := mustOK(t, NewReadFile(PathPolicy{}), argsJSON(t, map[string]any{"path": path}))
	if n := len([]rune(res.Content)); n > readMaxChars+200 {
		t.Fatalf("content is %d runes, want it capped near %d", n, readMaxChars)
	}
	if !strings.Contains(res.Content, "(Showing lines 1-") {
		t.Fatalf("cap footer missing: %q", res.Content[len(res.Content)-120:])
	}
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

func TestWriteFileCreatesParentsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFile(PathPolicy{Workspace: dir})

	content := "héllo\nworld\n"
	res := mustOK(t, tool, argsJSON(t, map[string]any{"path": "deep/nested/file.txt", "content": content}))
	if !strings.Contains(res.Content, "Successfully wrote 12 characters to") {
		t.Fatalf("write content = %q", res.Content)
	}
	target := filepath.Join(dir, "deep", "nested", "file.txt")
	if got := readFile(t, target); got != content {
		t.Fatalf("round trip = %q, want %q", got, content)
	}

	// Overwriting is allowed (filesystem.py:550-555).
	mustOK(t, tool, argsJSON(t, map[string]any{"path": "deep/nested/file.txt", "content": "short"}))
	if got := readFile(t, target); got != "short" {
		t.Fatalf("overwrite = %q", got)
	}

	// The content is readable back through read_file.
	read := mustOK(t, NewReadFile(PathPolicy{Workspace: dir}), argsJSON(t, map[string]any{"path": "deep/nested/file.txt"}))
	if !strings.HasPrefix(read.Content, "1| short") {
		t.Fatalf("read back = %q", read.Content)
	}
}

func TestWriteFileAdversarial(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFile(PathPolicy{Workspace: dir, AllowedDir: dir})

	res := mustErr(t, tool, `{"path":"a.txt"}`)
	if !strings.Contains(res.Content, "missing required content") {
		t.Fatalf("missing content = %q", res.Content)
	}

	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": "../escape.txt", "content": "x"}))
	if !strings.Contains(res.Content, "outside allowed directory") {
		t.Fatalf("boundary content = %q", res.Content)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Fatal("boundary violation wrote a file outside the workspace")
	}

	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": "", "content": "x"}))
	if !strings.Contains(res.Content, "Unknown path") {
		t.Fatalf("empty path content = %q", res.Content)
	}

	// A directory target fails as an error result, not a panic.
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": "adir", "content": "x"}))
	if !strings.Contains(res.Content, "Error writing file") {
		t.Fatalf("dir target content = %q", res.Content)
	}
}

// ---------------------------------------------------------------------------
// edit_file
// ---------------------------------------------------------------------------

func TestEditFileSuccessReplacesExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "hello world\n")

	res := mustOK(t, NewEditFile(PathPolicy{Workspace: dir}), argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "world", "new_text": "there",
	}))
	if !strings.Contains(res.Content, "Patch applied:\n- update a.txt") {
		t.Fatalf("summary = %q", res.Content)
	}
	got := readFile(t, path)
	if got != "hello there\n" {
		t.Fatalf("content = %q", got)
	}
	if strings.Count(got, "there") != 1 {
		t.Fatalf("replacement count = %d, want 1", strings.Count(got, "there"))
	}
}

func TestEditFileAdversarial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "one\ntwo\none\n")
	tool := NewEditFile(PathPolicy{Workspace: dir})

	// Not found is an observable error (filesystem.py:954-955, :1080).
	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "zzz", "new_text": "x",
	}))
	if !strings.Contains(res.Content, "old_text not found in a.txt") {
		t.Fatalf("not found = %q", res.Content)
	}

	// Ambiguous match must fail rather than guess (filesystem.py:968-978).
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "one", "new_text": "x",
	}))
	if !strings.Contains(res.Content, "old_text appears 2 times") {
		t.Fatalf("ambiguous = %q", res.Content)
	}
	if got := readFile(t, path); got != "one\ntwo\none\n" {
		t.Fatalf("ambiguous edit changed the file: %q", got)
	}

	// new_text == old_text is rejected (filesystem.py:918-919).
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "two", "new_text": "two",
	}))
	if !strings.Contains(res.Content, "must be different from old_text") {
		t.Fatalf("same text = %q", res.Content)
	}

	// Missing file without create semantics.
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "missing.txt", "old_text": "a", "new_text": "b",
	}))
	if !strings.Contains(res.Content, "File not found") {
		t.Fatalf("missing file = %q", res.Content)
	}

	// Mutual exclusion and range guards (filesystem.py:957-967).
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "one", "new_text": "x", "replace_all": true, "occurrence": 1,
	}))
	if !strings.Contains(res.Content, "occurrence cannot be used with replace_all=true") {
		t.Fatalf("mutual exclusion = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "one", "new_text": "x", "occurrence": 9,
	}))
	if !strings.Contains(res.Content, "out of range") {
		t.Fatalf("occurrence range = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "one", "new_text": "x", "line_hint": 2,
	}))
	if !strings.Contains(res.Content, "does not match the old_text location") {
		t.Fatalf("line_hint = %q", res.Content)
	}

	// Boundary escape.
	res = mustErr(t, NewEditFile(PathPolicy{Workspace: dir, AllowedDir: dir}), argsJSON(t, map[string]any{
		"path": "../escape.txt", "old_text": "a", "new_text": "b",
	}))
	if !strings.Contains(res.Content, "outside allowed directory") {
		t.Fatalf("boundary = %q", res.Content)
	}
	// WorkspaceBoundaryError is a PermissionError upstream, so it takes the
	// "Error: " arm rather than the generic "Error editing file: " one
	// (filesystem.py:1037-1040). Pinned here as well as differentially.
	if !strings.HasPrefix(res.Content, "Error: ") {
		t.Fatalf("boundary prefix = %q, want an \"Error: \" prefix", res.Content)
	}
}

func TestEditFileSelectionModes(t *testing.T) {
	dir := t.TempDir()
	tool := NewEditFile(PathPolicy{Workspace: dir})

	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "one\ntwo\none\n")

	// occurrence selects the second match.
	mustOK(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "one", "new_text": "ONE", "occurrence": 2,
	}))
	if got := readFile(t, path); got != "one\ntwo\nONE\n" {
		t.Fatalf("occurrence content = %q", got)
	}

	// replace_all rewrites every match.
	mustOK(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "one", "new_text": "1", "replace_all": true,
	}))
	if got := readFile(t, path); got != "1\ntwo\nONE\n" {
		t.Fatalf("replace_all content = %q", got)
	}

	// line_hint selects the match covering that line.
	writeFile(t, path, "x\ny\nx\n")
	mustOK(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "x", "new_text": "Z", "line_hint": 3,
	}))
	if got := readFile(t, path); got != "x\ny\nZ\n" {
		t.Fatalf("line_hint content = %q", got)
	}

	// expected_replacements guards the edit (filesystem.py:1009-1013).
	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "a.txt", "old_text": "Z", "new_text": "Q", "expected_replacements": 2,
	}))
	if !strings.Contains(res.Content, "expected 2 replacements but would make 1") {
		t.Fatalf("expected_replacements = %q", res.Content)
	}
}

func TestEditFileCreateSemanticsAndFormatting(t *testing.T) {
	dir := t.TempDir()
	tool := NewEditFile(PathPolicy{Workspace: dir})

	// old_text="" creates a new file (filesystem.py:921-928).
	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"path": "new/deep.txt", "old_text": "", "new_text": "hi",
	}))
	if !strings.Contains(res.Content, "Patch applied:\n- add new/deep.txt") {
		t.Fatalf("create summary = %q", res.Content)
	}
	if got := readFile(t, filepath.Join(dir, "new/deep.txt")); got != "hi" {
		t.Fatalf("created content = %q", got)
	}

	// old_text="" against a non-empty file is refused (filesystem.py:938-943).
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"path": "new/deep.txt", "old_text": "", "new_text": "other",
	}))
	if !strings.Contains(res.Content, "already exists and is not empty") {
		t.Fatalf("existing non-empty = %q", res.Content)
	}

	// Trailing whitespace is stripped for non-markdown files
	// (filesystem.py:982-984).
	writeFile(t, filepath.Join(dir, "t.txt"), "a\n")
	mustOK(t, tool, argsJSON(t, map[string]any{"path": "t.txt", "old_text": "a", "new_text": "b   "}))
	if got := readFile(t, filepath.Join(dir, "t.txt")); got != "b\n" {
		t.Fatalf("trailing ws content = %q", got)
	}

	// ... but preserved for markdown (filesystem.py:865, :983).
	writeFile(t, filepath.Join(dir, "t.md"), "a\n")
	mustOK(t, tool, argsJSON(t, map[string]any{"path": "t.md", "old_text": "a", "new_text": "b  "}))
	if got := readFile(t, filepath.Join(dir, "t.md")); got != "b  \n" {
		t.Fatalf("markdown content = %q", got)
	}

	// CRLF files stay CRLF (filesystem.py:949-950, :1031-1032).
	writeFile(t, filepath.Join(dir, "crlf.txt"), "a\r\nb\r\n")
	mustOK(t, tool, argsJSON(t, map[string]any{"path": "crlf.txt", "old_text": "b", "new_text": "c"}))
	if got := readFile(t, filepath.Join(dir, "crlf.txt")); got != "a\r\nc\r\n" {
		t.Fatalf("crlf content = %q", got)
	}

	// Deleting a whole line consumes its newline (filesystem.py:1019-1028).
	writeFile(t, filepath.Join(dir, "del.txt"), "keep\ndrop\nkeep2\n")
	mustOK(t, tool, argsJSON(t, map[string]any{"path": "del.txt", "old_text": "drop\n", "new_text": ""}))
	if got := readFile(t, filepath.Join(dir, "del.txt")); got != "keep\nkeep2\n" {
		t.Fatalf("deletion content = %q", got)
	}
}

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

func TestListDirSortedAndIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	if err := os.MkdirAll(filepath.Join(dir, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ignored := range []string{".git", "node_modules", "__pycache__"} {
		writeFile(t, filepath.Join(dir, ignored, "x"), "x")
	}

	res := mustOK(t, NewListDir(PathPolicy{Workspace: dir}), argsJSON(t, map[string]any{"path": "."}))
	// sorted(dp.iterdir()) orders by name, so the entry kind only affects the
	// rendered prefix, not the position (filesystem.py:1151-1157).
	want := "📄 a.txt\n📄 b.txt\n📁 c"
	if res.Content != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}

	// Deterministic across repeated calls.
	for i := 0; i < 3; i++ {
		if got := mustOK(t, NewListDir(PathPolicy{Workspace: dir}), argsJSON(t, map[string]any{"path": "."})); got.Content != want {
			t.Fatalf("call %d = %q, want %q", i, got.Content, want)
		}
	}
}

func TestListDirRecursiveAndMaxEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top.txt"), "t")
	writeFile(t, filepath.Join(dir, "sub", "f.txt"), "f")
	writeFile(t, filepath.Join(dir, "sub", "build", "ignored.txt"), "i")
	tool := NewListDir(PathPolicy{Workspace: dir})

	res := mustOK(t, tool, argsJSON(t, map[string]any{"path": ".", "recursive": true}))
	want := "sub/\nsub/f.txt\ntop.txt"
	if res.Content != want {
		t.Fatalf("recursive content = %q, want %q", res.Content, want)
	}

	res = mustOK(t, tool, argsJSON(t, map[string]any{"path": ".", "recursive": true, "max_entries": 1}))
	if !strings.HasPrefix(res.Content, "sub/\n\n(truncated, showing first 1 of 3 entries)") {
		t.Fatalf("max_entries content = %q", res.Content)
	}
}

func TestListDirAdversarial(t *testing.T) {
	dir := t.TempDir()
	tool := NewListDir(PathPolicy{Workspace: dir})

	res := mustErr(t, tool, argsJSON(t, map[string]any{"path": "missing"}))
	if !strings.Contains(res.Content, "Directory not found") {
		t.Fatalf("missing dir = %q", res.Content)
	}

	writeFile(t, filepath.Join(dir, "file.txt"), "x")
	res = mustErr(t, tool, argsJSON(t, map[string]any{"path": "file.txt"}))
	if !strings.Contains(res.Content, "Not a directory") {
		t.Fatalf("not a dir = %q", res.Content)
	}

	res = mustErr(t, NewListDir(PathPolicy{Workspace: dir, AllowedDir: dir}), argsJSON(t, map[string]any{"path": ".."}))
	if !strings.Contains(res.Content, "outside allowed directory") {
		t.Fatalf("boundary = %q", res.Content)
	}

	// An empty directory is a success, not an error (filesystem.py:1159-1160).
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	res = mustOK(t, tool, argsJSON(t, map[string]any{"path": "empty"}))
	if !strings.Contains(res.Content, "is empty") {
		t.Fatalf("empty dir = %q", res.Content)
	}
}

// ---------------------------------------------------------------------------
// exec
// ---------------------------------------------------------------------------

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	tool := NewExec(ExecOptions{Workspace: t.TempDir()})

	res := mustOK(t, tool, argsJSON(t, map[string]any{"command": "echo hello"}))
	if !strings.Contains(res.Content, "hello") {
		t.Fatalf("stdout content = %q", res.Content)
	}
	if !strings.Contains(res.Content, "Exit code: 0") {
		t.Fatalf("exit code content = %q", res.Content)
	}

	// stderr is labelled (shell.py:331-333).
	res = mustOK(t, tool, argsJSON(t, map[string]any{"command": "echo oops >&2"}))
	if !strings.Contains(res.Content, "STDERR:\noops") {
		t.Fatalf("stderr content = %q", res.Content)
	}

	// The `cmd` alias is accepted (shell.py:283).
	res = mustOK(t, tool, argsJSON(t, map[string]any{"cmd": "echo alias"}))
	if !strings.Contains(res.Content, "alias") {
		t.Fatalf("cmd alias content = %q", res.Content)
	}

	// working_dir is honoured.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "here.txt"), "x")
	res = mustOK(t, tool, argsJSON(t, map[string]any{"command": "echo *.txt", "working_dir": dir}))
	if !strings.Contains(res.Content, "here.txt") {
		t.Fatalf("working_dir content = %q", res.Content)
	}
}

func TestExecNonZeroExitIsError(t *testing.T) {
	tool := NewExec(ExecOptions{Workspace: t.TempDir()})

	res := mustErr(t, tool, argsJSON(t, map[string]any{"command": "echo before; exit 3"}))
	if !strings.Contains(res.Content, "before") {
		t.Fatalf("content = %q", res.Content)
	}
	if !strings.Contains(res.Content, "Exit code: 3") {
		t.Fatalf("exit code = %q", res.Content)
	}
}

func TestExecMissingCommandAndBadParams(t *testing.T) {
	tool := NewExec(ExecOptions{Workspace: t.TempDir()})

	res := mustErr(t, tool, `{}`)
	if !strings.Contains(res.Content, "Missing command") {
		t.Fatalf("missing command = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"command": "echo x", "bogus": true}))
	if !strings.Contains(res.Content, "unexpected parameter bogus") {
		t.Fatalf("unknown param = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"command": "echo x", "shell": "powershell"}))
	if !strings.Contains(res.Content, "unsupported shell") {
		t.Fatalf("shell validation = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"command": "echo x", "shell": "/bin/false"}))
	if !strings.Contains(res.Content, "unsupported shell") {
		t.Fatalf("absolute shell validation = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"command": "echo x", "yield_time_ms": 1000}))
	if !strings.Contains(res.Content, "yield_time_ms") {
		t.Fatalf("yield_time_ms = %q", res.Content)
	}

	// A working directory that does not exist fails at spawn as an error
	// result, not as a Go error or a panic.
	res = mustErr(t, tool, argsJSON(t, map[string]any{
		"command": "echo x", "working_dir": filepath.Join(t.TempDir(), "nope"),
	}))
	if !strings.Contains(res.Content, "Error executing command") {
		t.Fatalf("bad working_dir = %q", res.Content)
	}
}

func TestExecWorkspaceGuard(t *testing.T) {
	dir := t.TempDir()
	tool := NewExec(ExecOptions{Workspace: dir, RestrictToWorkspace: true})

	res := mustErr(t, tool, argsJSON(t, map[string]any{"command": "echo x", "working_dir": "../.."}))
	if !strings.Contains(res.Content, "working_dir is outside the configured workspace") {
		t.Fatalf("working_dir guard = %q", res.Content)
	}
	if !strings.Contains(res.Content, "hard policy boundary") {
		t.Fatalf("boundary note missing: %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"command": "rm -rf /tmp/nanobot-nope"}))
	if !strings.Contains(res.Content, "deny pattern filter") {
		t.Fatalf("deny pattern = %q", res.Content)
	}
	res = mustErr(t, tool, argsJSON(t, map[string]any{"command": "cat ../etc/passwd"}))
	if !strings.Contains(res.Content, "path traversal detected") {
		t.Fatalf("traversal = %q", res.Content)
	}

	// Without restriction the same commands are not filtered (the reference
	// only guards when restrict_to_workspace is on, shell.py:446-455).
	open := NewExec(ExecOptions{Workspace: dir})
	mustOK(t, open, argsJSON(t, map[string]any{"command": "echo unguarded"}))
}

// TestExecTimeoutKillsProcess asserts the hard timeout is enforced and the
// child (and its process group) is killed promptly (shell.py:308-315, :697-730).
func TestExecTimeoutKillsProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	tool := NewExec(ExecOptions{Workspace: dir})

	start := time.Now()
	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"command": fmt.Sprintf("echo $$ > %q; while :; do :; done", pidFile),
		"timeout": 1,
	}))
	elapsed := time.Since(start)

	if !strings.Contains(res.Content, "timed out after 1 seconds") {
		t.Fatalf("timeout content = %q", res.Content)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout took %s, want prompt cancellation", elapsed)
	}
	assertProcessGone(t, pidFile, elapsed)
}

// TestExecCancellationKillsProcess asserts a canceled context aborts the run and
// kills the child rather than leaving it running.
func TestExecCancellationKillsProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	tool := NewExec(ExecOptions{Workspace: dir})

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res tools.Result
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := tool.Execute(ctx, json.RawMessage(argsJSON(t, map[string]any{
			"command": fmt.Sprintf("echo $$ > %q; while :; do :; done", pidFile),
			"timeout": 300,
		})))
		done <- outcome{res, err}
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", got.err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("cancellation took %s", time.Since(start))
		}
		assertProcessGone(t, pidFile, time.Since(start))
	case <-time.After(20 * time.Second):
		t.Fatal("Execute did not return after cancellation")
	}
}

// assertProcessGone verifies the child recorded in pidFile is no longer alive.
// The check is Linux-specific; on other platforms it is skipped.
func assertProcessGone(t *testing.T, pidFile string, elapsed time.Duration) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("process liveness check is Linux-specific")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still alive %s after the run ended (kill err=%v)", pid, elapsed, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExecOutputCapped asserts both the character cap (shell.py:339-346) and the
// internal byte capture cap hold a runaway command's output in check.
func TestExecOutputCapped(t *testing.T) {
	tool := NewExec(ExecOptions{Workspace: t.TempDir()})

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"command": "head -c 2000000 /dev/zero | tr '\\0' 'x'",
	}))
	if n := len([]rune(res.Content)); n > execMaxOutputChars+200 {
		t.Fatalf("content is %d runes, want it capped near %d", n, execMaxOutputChars)
	}
	if !strings.Contains(res.Content, "chars truncated") {
		t.Fatalf("char truncation marker missing: %q", tail(res.Content, 200))
	}
	if !strings.Contains(res.Content, "capture capped at") {
		t.Fatalf("byte capture cap marker missing: %q", tail(res.Content, 300))
	}

	// An explicit session output limit is clamped to [1000, 50000]
	// (shell.py:339, exec_session.py:466-469).
	res = mustOK(t, tool, argsJSON(t, map[string]any{
		"command":          "head -c 200000 /dev/zero | tr '\\0' 'y'",
		"max_output_chars": 2000,
	}))
	if n := len([]rune(res.Content)); n > 2000+200 {
		t.Fatalf("content is %d runes with max_output_chars=2000", n)
	}

	// A huge value is clamped to 50000 rather than honoured.
	res = mustOK(t, tool, argsJSON(t, map[string]any{
		"command":          "head -c 200000 /dev/zero | tr '\\0' 'y'",
		"max_output_chars": 1000000,
	}))
	if n := len([]rune(res.Content)); n > execMaxOutputCharsLimit+200 {
		t.Fatalf("content is %d runes, want the 50000 clamp", n)
	}
}

func TestExecTimeoutClampedToMax(t *testing.T) {
	// A model-supplied timeout above the cap is clamped (shell.py:386-398), so a
	// short command still completes.
	tool := NewExec(ExecOptions{Workspace: t.TempDir()})
	mustOK(t, tool, argsJSON(t, map[string]any{"command": "echo ok", "timeout": 100000}))
}

func TestExecDefaultTimeout(t *testing.T) {
	tool := NewExec(ExecOptions{Workspace: t.TempDir()})
	if got := tool.resolveTimeout(0, false); got != 60*time.Second {
		t.Fatalf("default timeout = %s, want 60s", got)
	}
	if got := tool.resolveTimeout(5, true); got != 5*time.Second {
		t.Fatalf("explicit timeout = %s, want 5s", got)
	}
	if got := tool.resolveTimeout(100000, true); got != 600*time.Second {
		t.Fatalf("clamped timeout = %s, want 600s", got)
	}
	custom := NewExec(ExecOptions{DefaultTimeout: 90 * time.Second})
	if got := custom.resolveTimeout(0, false); got != 90*time.Second {
		t.Fatalf("configured default = %s, want 90s", got)
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
