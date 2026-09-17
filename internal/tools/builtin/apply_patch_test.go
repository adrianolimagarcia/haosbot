package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The expected strings in this file were produced by executing the frozen
// reference (nanobot/agent/tools/apply_patch.py at
// HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9) through
// compat/python/dump_apply_patch.py. They are reproduced here so the tool can
// be checked without the reference venv; compat/apply_patch_differential_test.go
// re-derives them from the reference on every run.

func newPatchTool(t *testing.T) (*ApplyPatch, string) {
	t.Helper()
	ws := t.TempDir()
	return NewApplyPatch(PathPolicy{Workspace: ws, AllowedDir: ws}), ws
}

func TestApplyPatchAddNewFile(t *testing.T) {
	tool, ws := newPatchTool(t)

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{"path": "new.txt", "action": "add", "new_text": "hello"}},
	}))
	if want := "Patch applied:\n- add new.txt (+1/-0)"; res.Content != want {
		t.Fatalf("Content = %q, want %q", res.Content, want)
	}
	if got := readFile(t, filepath.Join(ws, "new.txt")); got != "hello\n" {
		t.Fatalf("file content = %q, want %q", got, "hello\n")
	}

	// The diff is attached under the RESOLVED path, which is what the reference
	// keys its file_diffs dict by.
	key := filepath.Join(ws, "new.txt")
	diff, ok := res.FileDiffs[key]
	if !ok {
		t.Fatalf("FileDiffs has no entry for %q: %v", key, res.FileDiffs)
	}
	if diff.Added != 1 || diff.Deleted != 0 {
		t.Fatalf("diff = (+%d/-%d), want (+1/-0)", diff.Added, diff.Deleted)
	}
}

func TestApplyPatchAddExistingFileAppends(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "one\ntwo")

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{"path": "a.txt", "action": "add", "new_text": "three\n"}},
	}))
	// The action is "update", not "add", and the missing trailing newline is
	// inserted before the addition rather than merged into the last line.
	if want := "Patch applied:\n- update a.txt (+1/-0)"; res.Content != want {
		t.Fatalf("Content = %q, want %q", res.Content, want)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "one\ntwo\nthree\n" {
		t.Fatalf("file content = %q, want %q", got, "one\ntwo\nthree\n")
	}
}

// TestApplyPatchAddPreservesCRLF pins the asymmetry in the reference: appending
// to an existing CRLF file restores CRLF, while adding a NEW file does not.
func TestApplyPatchAddPreservesCRLF(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "one\r\ntwo\r\n")

	mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{"path": "a.txt", "action": "add", "new_text": "three\n"}},
	}))
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "one\r\ntwo\r\nthree\r\n" {
		t.Fatalf("existing file = %q, want CRLF preserved", got)
	}

	mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{"path": "new.txt", "action": "add", "new_text": "a\r\nb\r\n"}},
	}))
	// A new file is normalised to \n; only an existing file keeps CRLF.
	if got := readFile(t, filepath.Join(ws, "new.txt")); got != "a\nb\n" {
		t.Fatalf("new file = %q, want CRLF normalised to \\n", got)
	}
}

func TestApplyPatchReplace(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "hello world\n")

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{
			"path": "a.txt", "action": "replace", "old_text": "world", "new_text": "there",
		}},
	}))
	if want := "Patch applied:\n- update a.txt (+1/-1)"; res.Content != want {
		t.Fatalf("Content = %q, want %q", res.Content, want)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "hello there\n" {
		t.Fatalf("file content = %q", got)
	}
}

// TestApplyPatchReplaceAmbiguousSearchesFromPosPlusOne pins that the second
// search starts one character after the first match, so overlapping occurrences
// still count as ambiguous. "aa" in "aaa" is the minimal witness.
func TestApplyPatchReplaceAmbiguousSearchesFromPosPlusOne(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "aaa\n")

	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{
			"path": "a.txt", "action": "replace", "old_text": "aa", "new_text": "b",
		}},
	}))
	if want := "Error applying patch: old_text appears multiple times in a.txt"; res.Content != want {
		t.Fatalf("Content = %q, want %q", res.Content, want)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "aaa\n" {
		t.Fatalf("file was modified: %q", got)
	}
}

// TestApplyPatchReplaceNormalisesCRLF pins that the search happens on
// LF-normalised text and the file's original line endings are restored.
func TestApplyPatchReplaceNormalisesCRLF(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "one\r\ntwo\r\n")

	mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{{
			// old_text uses LF even though the file uses CRLF.
			"path": "a.txt", "action": "replace", "old_text": "one\ntwo", "new_text": "x",
		}},
	}))
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "x\r\n" {
		t.Fatalf("file content = %q, want %q", got, "x\r\n")
	}
}

func TestApplyPatchMultiFileSummaryOrder(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "a1\n")
	writeFile(t, filepath.Join(ws, "b.txt"), "b1\n")

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "action": "replace", "old_text": "a1", "new_text": "a2"},
			{"path": "b.txt", "action": "add", "new_text": "b2\n"},
			{"path": "c.txt", "action": "add", "new_text": "c1\n"},
		},
	}))
	// Summaries follow INSERTION order, which is why patchWrites keeps one.
	want := "Patch applied:\n- update a.txt (+1/-1)\n- update b.txt (+1/-0)\n- add c.txt (+1/-0)"
	if res.Content != want {
		t.Fatalf("Content\n got %q\nwant %q", res.Content, want)
	}
	if len(res.FileDiffs) != 3 {
		t.Fatalf("FileDiffs has %d entries, want 3", len(res.FileDiffs))
	}
}

// TestApplyPatchSecondEditSeesPendingContent pins that a later edit to the same
// file reads the pending content, while the reported diff is still measured
// against the file's ORIGINAL text.
func TestApplyPatchSecondEditSeesPendingContent(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "one\n")

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "action": "replace", "old_text": "one", "new_text": "two"},
			{"path": "a.txt", "action": "replace", "old_text": "two", "new_text": "three"},
		},
	}))
	if want := "Patch applied:\n- update a.txt (+1/-1)"; res.Content != want {
		t.Fatalf("Content = %q, want %q", res.Content, want)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "three\n" {
		t.Fatalf("file content = %q, want %q", got, "three\n")
	}
	diff := res.FileDiffs[filepath.Join(ws, "a.txt")]
	if diff.Added != 1 || diff.Deleted != 1 {
		t.Fatalf("diff = (+%d/-%d), want (+1/-1) against the original text", diff.Added, diff.Deleted)
	}
}

func TestApplyPatchDryRunWritesNothing(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "one\n")

	res := mustOK(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "action": "replace", "old_text": "one", "new_text": "1"},
			{"path": "b.txt", "action": "add", "new_text": "new\n"},
		},
		"dry_run": true,
	}))
	want := "Patch dry-run succeeded:\n- update a.txt (+1/-1)\n- add b.txt (+1/-0)"
	if res.Content != want {
		t.Fatalf("Content\n got %q\nwant %q", res.Content, want)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "one\n" {
		t.Fatalf("dry run modified a.txt: %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "b.txt")); err == nil {
		t.Fatal("dry run created b.txt")
	}
	// The reference returns a plain str for a dry run, not a FileEditResult, so
	// no diffs are attached.
	if res.FileDiffs != nil {
		t.Fatalf("dry run attached FileDiffs: %v", res.FileDiffs)
	}
}

func TestApplyPatchDryRunStillValidates(t *testing.T) {
	tool, _ := newPatchTool(t)
	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"edits":   []map[string]any{{"path": "a.txt", "action": "replace", "old_text": "x", "new_text": "y"}},
		"dry_run": true,
	}))
	if want := "Error applying patch: file to update does not exist: a.txt"; res.Content != want {
		t.Fatalf("Content = %q, want %q", res.Content, want)
	}
}

// TestApplyPatchRollback pins the transactional write: a failure on the LAST
// target must undo the earlier writes, restoring modified files and deleting
// created ones. The failure is triggered deterministically by making one
// target's parent an existing regular file.
func TestApplyPatchRollback(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "original\n")
	writeFile(t, filepath.Join(ws, "blocker"), "not a directory\n")

	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "action": "replace", "old_text": "original", "new_text": "changed"},
			{"path": "b.txt", "action": "add", "new_text": "brand new\n"},
			{"path": "blocker/x.txt", "action": "add", "new_text": "doomed\n"},
		},
	}))
	// The wording of the underlying failure is Go's, not Python's; the port
	// reports the OS error as-is.
	if !strings.HasPrefix(res.Content, "Error applying patch: ") {
		t.Fatalf("Content = %q, want an \"Error applying patch: \" prefix", res.Content)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "original\n" {
		t.Fatalf("rollback did not restore a.txt: %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "b.txt")); err == nil {
		t.Fatal("rollback did not delete the created b.txt")
	}
	if got := readFile(t, filepath.Join(ws, "blocker")); got != "not a directory\n" {
		t.Fatalf("blocker was modified: %q", got)
	}
}

func TestApplyPatchRejectsInvalidEdits(t *testing.T) {
	tool, ws := newPatchTool(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "one\n")
	writeFile(t, filepath.Join(ws, "raw.bin"), "caf\xe9\n")

	tests := []struct {
		name string
		args any
		want string
	}{
		{
			name: "no_edits",
			args: map[string]any{},
			want: "Error applying patch: must provide edits",
		},
		{
			name: "empty_edits",
			args: map[string]any{"edits": []any{}},
			want: "Error applying patch: must provide edits",
		},
		{
			name: "edits_null",
			args: map[string]any{"edits": nil},
			want: "Error applying patch: must provide edits",
		},
		{
			name: "edits_not_iterable",
			args: map[string]any{"edits": 5},
			want: "Error applying patch: 'int' object is not iterable",
		},
		{
			name: "edits_bool_not_iterable",
			args: map[string]any{"edits": true},
			want: "Error applying patch: 'bool' object is not iterable",
		},
		{
			name: "edit_not_object",
			args: map[string]any{"edits": []any{1}},
			want: "Error applying patch: each edit must be an object",
		},
		{
			name: "edit_string_not_object",
			args: map[string]any{"edits": []any{"x"}},
			want: "Error applying patch: each edit must be an object",
		},
		{
			name: "edit_null_not_object",
			args: map[string]any{"edits": []any{nil}},
			want: "Error applying patch: each edit must be an object",
		},
		{
			name: "missing_path",
			args: map[string]any{"edits": []any{map[string]any{"action": "add", "new_text": "x"}}},
			want: "Error applying patch: path required for edit",
		},
		{
			name: "path_wrong_type",
			args: map[string]any{"edits": []any{map[string]any{"path": 5, "action": "add", "new_text": "x"}}},
			want: "Error applying patch: path required for edit",
		},
		{
			name: "blank_path",
			args: map[string]any{"edits": []any{map[string]any{"path": "   ", "action": "add", "new_text": "x"}}},
			want: "Error applying patch: patch path cannot be empty",
		},
		{
			name: "nbsp_only_path",
			args: map[string]any{"edits": []any{map[string]any{"path": "\u00a0", "action": "add", "new_text": "x"}}},
			want: "Error applying patch: patch path cannot be empty",
		},
		{
			name: "null_byte_path",
			args: map[string]any{"edits": []any{map[string]any{"path": "a\x00b", "action": "add", "new_text": "x"}}},
			want: "Error applying patch: patch path contains a null byte: 'a\\x00b'",
		},
		{
			name: "null_byte_path_double_quoted_repr",
			args: map[string]any{"edits": []any{map[string]any{"path": "a'b\x00c", "action": "add", "new_text": "x"}}},
			want: `Error applying patch: patch path contains a null byte: "a'b\x00c"`,
		},
		{
			name: "missing_action",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt"}}},
			want: "Error applying patch: action required for edit: a.txt",
		},
		{
			name: "action_wrong_type",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt", "action": 3}}},
			want: "Error applying patch: action required for edit: a.txt",
		},
		{
			name: "action_null",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt", "action": nil}}},
			want: "Error applying patch: action required for edit: a.txt",
		},
		{
			name: "unknown_action",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt", "action": "delete"}}},
			want: "Error applying patch: unknown action: delete",
		},
		{
			name: "add_missing_new_text",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt", "action": "add"}}},
			want: "Error applying patch: new_text required for add: a.txt",
		},
		{
			name: "add_null_new_text",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt", "action": "add", "new_text": nil}}},
			want: "Error applying patch: new_text required for add: a.txt",
		},
		{
			name: "add_non_string_new_text",
			args: map[string]any{"edits": []any{map[string]any{"path": "a.txt", "action": "add", "new_text": 5}}},
			want: "Error applying patch: 'int' object has no attribute 'replace'",
		},
		{
			name: "replace_missing_file",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "nope.txt", "action": "replace", "old_text": "a", "new_text": "b",
			}}},
			want: "Error applying patch: file to update does not exist: nope.txt",
		},
		{
			name: "replace_missing_old_text",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "new_text": "b",
			}}},
			want: "Error applying patch: old_text required for replace: a.txt",
		},
		{
			name: "replace_zero_old_text",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "old_text": 0, "new_text": "b",
			}}},
			want: "Error applying patch: old_text required for replace: a.txt",
		},
		{
			name: "replace_empty_old_text",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "old_text": "", "new_text": "b",
			}}},
			want: "Error applying patch: old_text required for replace: a.txt",
		},
		{
			name: "replace_missing_new_text",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "old_text": "one",
			}}},
			want: "Error applying patch: new_text required for replace: a.txt",
		},
		{
			name: "replace_null_new_text",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "old_text": "one", "new_text": nil,
			}}},
			want: "Error applying patch: new_text required for replace: a.txt",
		},
		{
			name: "replace_non_string_new_text",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "old_text": "one", "new_text": 5,
			}}},
			want: "Error applying patch: 'int' object has no attribute 'replace'",
		},
		{
			name: "replace_not_found",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "a.txt", "action": "replace", "old_text": "zzz", "new_text": "b",
			}}},
			want: "Error applying patch: old_text not found in a.txt",
		},
		{
			name: "non_utf8_add",
			args: map[string]any{"edits": []any{map[string]any{
				"path": "raw.bin", "action": "add", "new_text": "x\n",
			}}},
			want: "Error applying patch: file is not UTF-8 text: raw.bin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := mustErr(t, tool, argsJSON(t, tc.args))
			if res.Content != tc.want {
				t.Fatalf("Content\n got %q\nwant %q", res.Content, tc.want)
			}
		})
	}

	// Nothing above may have written anything.
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "one\n" {
		t.Fatalf("a.txt was modified by a rejected patch: %q", got)
	}
}

// TestApplyPatchBoundaryViolationUsesErrorPrefix pins that a workspace escape
// is reported as a PermissionError, which the reference catches before its
// generic arm and therefore renders as "Error: ..." rather than
// "Error applying patch: ...".
func TestApplyPatchBoundaryViolationUsesErrorPrefix(t *testing.T) {
	tool, ws := newPatchTool(t)
	res := mustErr(t, tool, argsJSON(t, map[string]any{
		"edits": []any{map[string]any{"path": "../escape.txt", "action": "add", "new_text": "x"}},
	}))
	if !strings.HasPrefix(res.Content, "Error: ") {
		t.Fatalf("Content = %q, want an \"Error: \" prefix for a policy boundary", res.Content)
	}
	if strings.Contains(res.Content, "Error applying patch") {
		t.Fatalf("Content = %q, must not use the generic prefix", res.Content)
	}
	if !strings.Contains(res.Content, ws) {
		t.Fatalf("Content = %q, want it to name the workspace %q", res.Content, ws)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); err == nil {
		t.Fatal("a file was created outside the workspace")
	}
}

func TestApplyPatchParametersShape(t *testing.T) {
	tool, _ := newPatchTool(t)
	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing: %v", schema)
	}
	edits, ok := props["edits"].(map[string]any)
	if !ok {
		t.Fatalf("edits missing: %v", props)
	}
	if got := edits["minItems"]; got != json.Number("1") && got != float64(1) {
		t.Errorf("edits.minItems = %v, want 1", got)
	}
	if got := edits["maxItems"]; got != json.Number("20") && got != float64(20) {
		t.Errorf("edits.maxItems = %v, want 20", got)
	}

	// The inner edit object is deliberately NOT strict: the reference's
	// ObjectSchema leaves additional_properties unset there.
	items, ok := edits["items"].(map[string]any)
	if !ok {
		t.Fatalf("edits.items missing: %v", edits)
	}
	if _, present := items["additionalProperties"]; present {
		t.Errorf("edits.items must not declare additionalProperties: %v", items)
	}
	required, ok := items["required"].([]any)
	if !ok || len(required) != 2 || required[0] != "path" || required[1] != "action" {
		t.Errorf("edits.items.required = %v, want [path action]", items["required"])
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("edits.items.properties missing: %v", items)
	}
	for _, name := range []string{"path", "action", "old_text", "new_text"} {
		if _, ok := itemProps[name]; !ok {
			t.Errorf("edits.items.properties is missing %q", name)
		}
	}
	action, _ := itemProps["action"].(map[string]any)
	enum, _ := action["enum"].([]any)
	if len(enum) != 2 || enum[0] != "replace" || enum[1] != "add" {
		t.Errorf("action.enum = %v, want [replace add]", action["enum"])
	}

	dryRun, ok := props["dry_run"].(map[string]any)
	if !ok {
		t.Fatalf("dry_run missing: %v", props)
	}
	if dryRun["type"] != "boolean" {
		t.Errorf("dry_run.type = %v, want boolean", dryRun["type"])
	}
	if got := dryRun["default"]; got != false {
		t.Errorf("dry_run.default = %v, want false", got)
	}
}
