package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// listDirDefaultMax mirrors ListDirTool._DEFAULT_MAX (filesystem.py:1102).
const listDirDefaultMax = 200

// listDirIgnoreDirs mirrors ListDirTool._IGNORE_DIRS (filesystem.py:1103-1107).
var listDirIgnoreDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true,
	"dist": true, "build": true, ".tox": true, ".mypy_cache": true, ".pytest_cache": true,
	".ruff_cache": true, ".coverage": true, "htmlcov": true,
}

// ListDir lists directory contents with optional recursion.
//
// Ports ListDirTool (nanobot/agent/tools/filesystem.py:1087-1169). Read-only:
// embeds tools.ReadOnlyBase.
//
// NOTE ON THE TOOL NAME: the task brief called this tool "list_directory", but
// the reference name is "list_dir" (filesystem.py:1109-1111) and this
// repository's own agent prompt template also refers to `list_dir`
// (internal/prompt/templates/agent/tool_contract.md:21). The model calls tools
// by name, so the reference name is used.
type ListDir struct {
	tools.ReadOnlyBase
	policy PathPolicy
	params json.RawMessage
}

// NewListDir returns a list_dir tool bound to policy.
func NewListDir(policy PathPolicy) *ListDir {
	return &ListDir{
		policy: policy,
		// Mirrors the @tool_parameters decorator on ListDirTool
		// (filesystem.py:1087-1096).
		params: objectSchema(
			[]string{"path"},
			map[string]any{
				"path":        strProp("The directory path to list"),
				"recursive":   plainBoolProp("Recursively list all files (default false)"),
				"max_entries": minIntProp("Maximum entries to return (default 200)", 1),
			},
		),
	}
}

// Name mirrors ListDirTool.name (filesystem.py:1109-1111).
func (t *ListDir) Name() string { return "list_dir" }

// Description mirrors ListDirTool.description (filesystem.py:1113-1120) verbatim.
func (t *ListDir) Description() string {
	return "List the contents of a directory. " +
		"Set recursive=true to explore nested structure. " +
		"Common noise directories (.git, node_modules, __pycache__, etc.) are auto-ignored."
}

// Parameters returns the JSON Schema for the arguments (filesystem.py:1087-1096).
func (t *ListDir) Parameters() json.RawMessage { return t.params }

// Execute ports ListDirTool.execute (filesystem.py:1125-1169).
func (t *ListDir) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	args, bad := parseArgs(t.Name(), raw, "path", "recursive", "max_entries")
	if bad != nil {
		return *bad, nil
	}
	path, bad := requiredString(t.Name(), args, "path")
	if bad != nil {
		return *bad, nil
	}
	recursive, _, bad := optionalBool(t.Name(), args, "recursive")
	if bad != nil {
		return *bad, nil
	}
	maxEntries, hasMax, bad := optionalInt(t.Name(), args, "max_entries")
	if bad != nil {
		return *bad, nil
	}

	dp, err := t.policy.resolveRead(path)
	if err != nil {
		return permissionOrGenericList(err), nil
	}
	info, err := os.Stat(dp)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tools.Errf("Error: Directory not found: %s", path), nil
		}
		return permissionOrGenericList(err), nil
	}
	if !info.IsDir() {
		return tools.Errf("Error: Not a directory: %s", path), nil
	}

	limit := listDirDefaultMax
	if hasMax && maxEntries > 0 {
		limit = maxEntries
	}

	var items []string
	total := 0
	if recursive {
		var rels []string
		err := filepath.WalkDir(dp, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == dp {
				return nil
			}
			// The reference filters on the parts of the walked path, so an
			// ignored directory name anywhere in the path (including an
			// ancestor of dp) suppresses the entry (filesystem.py:1143-1145).
			if hasIgnoredPart(p) {
				return nil
			}
			rel, relErr := filepath.Rel(dp, p)
			if relErr != nil {
				return relErr
			}
			rels = append(rels, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return permissionOrGenericList(err), nil
		}
		// sorted(dp.rglob("*")) orders Path objects by their parts tuple
		// (PurePath._cparts); compare parts to match that ordering exactly.
		sort.Slice(rels, func(i, j int) bool {
			return lessPathParts(strings.Split(rels[i], "/"), strings.Split(rels[j], "/"))
		})
		for _, rel := range rels {
			total++
			if len(items) >= limit {
				continue
			}
			if isDirEntry(dp, rel) {
				items = append(items, rel+"/")
			} else {
				items = append(items, rel)
			}
		}
	} else {
		entries, err := os.ReadDir(dp)
		if err != nil {
			return permissionOrGenericList(err), nil
		}
		// os.ReadDir returns entries sorted by filename, which matches
		// sorted(dp.iterdir()) for a single directory.
		for _, entry := range entries {
			if listDirIgnoreDirs[entry.Name()] {
				continue
			}
			total++
			if len(items) >= limit {
				continue
			}
			prefix := "📄 "
			if isDirEntry(dp, entry.Name()) {
				prefix = "📁 "
			}
			items = append(items, prefix+entry.Name())
		}
	}

	if len(items) == 0 && total == 0 {
		return tools.OK(fmt.Sprintf("Directory %s is empty", path)), nil
	}
	result := strings.Join(items, "\n")
	if total > limit {
		result += fmt.Sprintf("\n\n(truncated, showing first %d of %d entries)", limit, total)
	}
	return tools.OK(result), nil
}

// hasIgnoredPart reports whether any component of path is an ignored directory
// name.
func hasIgnoredPart(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if listDirIgnoreDirs[part] {
			return true
		}
	}
	return false
}

// isDirEntry reports whether the directory-relative path is a directory.
// os.Stat follows symlinks, matching Path.is_dir() in the reference.
func isDirEntry(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.IsDir()
}

// lessPathParts orders slash-separated paths the way Python orders Path objects
// (component-wise, so "a/x" sorts before "a.txt").
func lessPathParts(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// permissionOrGenericList mirrors the reference's PermissionError / generic
// error handling (filesystem.py:1166-1169).
//
// WorkspaceBoundaryError is a PermissionError subclass upstream
// (security/workspace_policy.py:20), so a policy escape must take the
// reference's `except PermissionError` arm and render as "Error: ...".
// Verified by running the reference: list_dir on "../" yields
// "Error: Path ../ is outside allowed directory <dir> (...)".
func permissionOrGenericList(err error) tools.Result {
	var boundary *boundaryError
	if errors.As(err, &boundary) || errors.Is(err, fs.ErrPermission) {
		return tools.Errf("Error: %v", err)
	}
	return tools.Errf("Error listing directory: %v", err)
}
