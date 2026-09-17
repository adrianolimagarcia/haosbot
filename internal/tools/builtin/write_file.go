package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// WriteFile creates or replaces a whole file.
//
// Ports WriteFileTool (nanobot/agent/tools/filesystem.py:533-571). Mutating:
// embeds tools.Base, whose defaults are conservative (not read-only, not
// concurrency-safe, not exclusive).
type WriteFile struct {
	tools.Base
	policy PathPolicy
	params json.RawMessage
}

// NewWriteFile returns a write_file tool bound to policy.
func NewWriteFile(policy PathPolicy) *WriteFile {
	return &WriteFile{
		policy: policy,
		// Mirrors the @tool_parameters decorator on WriteFileTool
		// (filesystem.py:533-539).
		params: objectSchema(
			[]string{"path", "content"},
			map[string]any{
				"path":    strProp("The file path to write to"),
				"content": strProp("The content to write"),
			},
		),
	}
}

// Name mirrors WriteFileTool.name (filesystem.py:544-546).
func (t *WriteFile) Name() string { return "write_file" }

// Description mirrors WriteFileTool.description (filesystem.py:548-555) verbatim.
func (t *WriteFile) Description() string {
	return "Create a new file or intentionally replace an entire file with " +
		"the provided content. Overwrites existing files and creates parent " +
		"directories as needed. For code changes or partial edits, prefer " +
		"apply_patch; use edit_file only for small exact replacements."
}

// Parameters returns the JSON Schema for the arguments (filesystem.py:533-539).
func (t *WriteFile) Parameters() json.RawMessage { return t.params }

// Execute ports WriteFileTool.execute (filesystem.py:557-571).
func (t *WriteFile) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	args, bad := parseArgs(t.Name(), raw, "path", "content")
	if bad != nil {
		return *bad, nil
	}
	path, bad := requiredString(t.Name(), args, "path")
	if bad != nil {
		return *bad, nil
	}
	content, bad := requiredString(t.Name(), args, "content")
	if bad != nil {
		return *bad, nil
	}
	if path == "" {
		return tools.Errf("Error writing file: Unknown path"), nil
	}

	fp, err := t.policy.resolveWrite(path)
	if err != nil {
		return permissionOrGenericWrite(err), nil
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return permissionOrGenericWrite(err), nil
	}
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		return permissionOrGenericWrite(err), nil
	}
	// len(content) is the Python character count; runes match that for text.
	return tools.OK(fmt.Sprintf("Successfully wrote %d characters to %s", utf8.RuneCountInString(content), fp)), nil
}

// permissionOrGenericWrite mirrors the reference's PermissionError / generic
// error handling (filesystem.py:568-571).
//
// WorkspaceBoundaryError is a PermissionError subclass upstream
// (security/workspace_policy.py:20), so a policy escape must take the
// reference's `except PermissionError` arm and render as "Error: ...".
// Verified by running the reference: write_file on "../escape.txt" yields
// "Error: Path ../escape.txt is outside allowed directory <dir> (...)".
func permissionOrGenericWrite(err error) tools.Result {
	var boundary *boundaryError
	if errors.As(err, &boundary) || errors.Is(err, fs.ErrPermission) {
		return tools.Errf("Error: %v", err)
	}
	return tools.Errf("Error writing file: %v", err)
}
