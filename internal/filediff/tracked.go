package filediff

import (
	"path/filepath"
	"strings"
)

// TrackedFileEditTools mirrors TRACKED_FILE_EDIT_TOOLS
// (utils/file_edit_events.py:13). "apply_patch" is part of the set, which is why
// the port's file-edit tracking must know about the tool even when no tracker is
// interested in the diff itself.
var TrackedFileEditTools = map[string]bool{
	"write_file":  true,
	"edit_file":   true,
	"apply_patch": true,
}

// IsFileEditTool ports is_file_edit_tool (utils/file_edit_events.py:150-151).
//
// The reference guards with bool(tool_name) first, which only matters for None;
// the empty string is not in the set either way.
func IsFileEditTool(toolName string) bool { return TrackedFileEditTools[toolName] }

// DisplayFileEditPath ports display_file_edit_path
// (utils/file_edit_events.py:154-160).
//
// The path is shown relative to the workspace when it is inside it, and as an
// absolute POSIX path otherwise. Both sides are resolved non-strictly first,
// because the reference calls Path.resolve() on each: a workspace reached
// through a symlink must still contain its own files.
//
// An empty workspace means "no workspace configured", matching the reference's
// None: the absolute path is returned unchanged.
func DisplayFileEditPath(path, workspace string) string {
	if workspace != "" {
		if resolved, err := resolveNonStrict(path); err == nil {
			if root, err := resolveNonStrict(workspace); err == nil {
				if rel, err := filepath.Rel(root, resolved); err == nil && isWithinRel(rel) {
					return filepath.ToSlash(rel)
				}
			}
		}
	}
	return filepath.ToSlash(path)
}

// isWithinRel reports whether a filepath.Rel result stays inside the root.
// filepath.Rel returns a path starting with ".." exactly when the target is not
// a descendant, and Python's Path.relative_to raises in that case.
func isWithinRel(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveNonStrict mirrors Path.resolve(strict=False): symlinks are resolved for
// the longest existing prefix of the path and the non-existent remainder is
// appended verbatim. A path where nothing exists keeps its absolute logical
// form.
func resolveNonStrict(path string) (string, error) {
	dir, rest := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
