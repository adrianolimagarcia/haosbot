package builtin

import (
	"strings"
	"testing"
)

// The reference routes a workspace-boundary violation through its
// `except PermissionError` arm, because WorkspaceBoundaryError is a
// PermissionError subclass (security/workspace_policy.py:20). Every one of the
// four filesystem tools therefore renders a policy escape as "Error: ...", NOT
// as its tool-specific generic prefix.
//
// Verified by running the reference against a workspace escape
// (ReadFileTool/WriteFileTool/ListDirTool/EditFileTool with allowed_dir set to
// the workspace):
//
//	read_file   -> 'Error: Path ../escape.txt is outside allowed directory <dir> (...)'
//	write_file  -> 'Error: Path ../escape.txt is outside allowed directory <dir> (...)'
//	list_dir    -> 'Error: Path ../ is outside allowed directory <dir> (...)'
//	edit_file   -> 'Error: Path ../escape.txt is outside allowed directory <dir> (...)'
//
// The pre-existing boundary tests in builtin_test.go only assert
// strings.Contains(content, "outside allowed directory"), which is satisfied by
// BOTH the correct and the wrong prefix — which is exactly why the bug in
// read_file/write_file/list_dir survived. This test pins the prefix itself.
func TestWorkspaceBoundaryUsesPermissionPrefix(t *testing.T) {
	root := t.TempDir()

	// Each generic arm that must NOT be taken for a policy escape.
	cases := []struct {
		name       string
		tool       interface{ Name() string }
		run        func(t *testing.T, policy PathPolicy) string
		wrongArm   string
		expectPath string
	}{
		{
			name: "read_file",
			run: func(t *testing.T, p PathPolicy) string {
				return mustErr(t, NewReadFile(p), argsJSON(t, map[string]any{"path": "../escape.txt"})).Content
			},
			wrongArm:   "Error reading file: ",
			expectPath: "Path ../escape.txt is outside allowed directory",
		},
		{
			name: "write_file",
			run: func(t *testing.T, p PathPolicy) string {
				return mustErr(t, NewWriteFile(p), argsJSON(t, map[string]any{"path": "../escape.txt", "content": "x"})).Content
			},
			wrongArm:   "Error writing file: ",
			expectPath: "Path ../escape.txt is outside allowed directory",
		},
		{
			name: "list_dir",
			run: func(t *testing.T, p PathPolicy) string {
				return mustErr(t, NewListDir(p), argsJSON(t, map[string]any{"path": "../"})).Content
			},
			wrongArm:   "Error listing directory: ",
			expectPath: "Path ../ is outside allowed directory",
		},
		{
			name: "edit_file",
			run: func(t *testing.T, p PathPolicy) string {
				return mustErr(t, NewEditFile(p), argsJSON(t, map[string]any{
					"path": "../escape.txt", "old_text": "a", "new_text": "b",
				})).Content
			},
			wrongArm:   "Error editing file: ",
			expectPath: "Path ../escape.txt is outside allowed directory",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run(t, PathPolicy{Workspace: root, AllowedDir: root})

			// The discriminating assertion: the generic arm must not be taken.
			if strings.HasPrefix(got, tc.wrongArm) {
				t.Fatalf("%s took the generic arm for a policy escape\n got: %q\nwant prefix: \"Error: \"\nwrong prefix: %q",
					tc.name, got, tc.wrongArm)
			}
			if !strings.HasPrefix(got, "Error: ") {
				t.Fatalf("%s boundary escape = %q, want prefix %q", tc.name, got, "Error: ")
			}
			// And it must still be the boundary error, not some other "Error: ".
			if !strings.Contains(got, tc.expectPath) {
				t.Fatalf("%s boundary escape = %q, want it to contain %q", tc.name, got, tc.expectPath)
			}
			if !strings.Contains(got, "hard policy boundary") {
				t.Fatalf("%s missing the boundary note: %q", tc.name, got)
			}
		})
	}
}

// TestWorkspaceBoundaryGenericArmsStillReachable guards against "fixing" the
// prefix by deleting the generic arms: a non-boundary failure must still use
// its tool-specific prefix. read_file on a missing file keeps the reference's
// dedicated not-found message (filesystem.py:317).
func TestWorkspaceBoundaryGenericArmsStillReachable(t *testing.T) {
	root := t.TempDir()
	policy := PathPolicy{Workspace: root, AllowedDir: root}

	got := mustErr(t, NewReadFile(policy), argsJSON(t, map[string]any{"path": "missing.txt"})).Content
	if !strings.HasPrefix(got, "Error: File not found: ") {
		t.Fatalf("read_file missing = %q, want the reference's not-found message (filesystem.py:317)", got)
	}
}
