package builtin

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PathPolicy controls how the filesystem tools resolve a model-supplied path.
//
// It mirrors _FsTool (nanobot/agent/tools/filesystem.py:34-195) together with
// resolve_workspace_path (agent/tools/path_utils.py:9-26) and
// resolve_allowed_path (security/workspace_policy.py:99-...).
type PathPolicy struct {
	// Workspace resolves relative paths (access.project_path,
	// filesystem.py:142-149). Empty means "resolve against the process working
	// directory", matching Path.resolve().
	Workspace string
	// AllowedDir is the workspace boundary. Empty means unrestricted, i.e.
	// _restrict_to_workspace is false (filesystem.py:71-75).
	AllowedDir string
	// ExtraReadDirs/ExtraReadFiles/ExtraWriteDirs/ExtraWriteFiles mirror
	// extra_allowed_dirs / extra_read_allowed_files / extra_write_allowed_dirs /
	// extra_write_allowed_files (filesystem.py:64-70).
	ExtraReadDirs   []string
	ExtraReadFiles  []string
	ExtraWriteDirs  []string
	ExtraWriteFiles []string
}

// workspaceBoundaryNote is appended to boundary errors. Verbatim from
// security/workspace_policy.py:12-16.
const workspaceBoundaryNote = " (this is a hard policy boundary, not a transient failure; " +
	"do not retry with shell tricks or alternative tools, and ask " +
	"the user how to proceed if the resource is genuinely required)"

// boundaryError mirrors WorkspaceBoundaryError (workspace_policy.py:17-23).
// The reference filesystem tools catch it as a PermissionError and turn it into
// `Error: <message>` (filesystem.py:412-414, :568-570, :1037-1039).
type boundaryError struct{ msg string }

func (e *boundaryError) Error() string { return e.msg + workspaceBoundaryNote }

// resolveRead resolves a read path (filesystem.py:151-180). The extra file
// allow-list only applies when a boundary is configured
// (extra_files_require_allowed_root=True).
func (p PathPolicy) resolveRead(path string) (string, error) {
	return p.resolve(path, p.ExtraReadDirs, p.ExtraReadFiles, true)
}

// resolveWrite resolves a write path (filesystem.py:182-188).
func (p PathPolicy) resolveWrite(path string) (string, error) {
	return p.resolve(path, p.ExtraWriteDirs, p.ExtraWriteFiles, false)
}

func (p PathPolicy) resolve(path string, extraDirs, extraFiles []string, requireRootForFiles bool) (string, error) {
	logical, err := p.logicalPath(path)
	if err != nil {
		return "", err
	}
	resolved, err := resolveNonStrict(logical)
	if err != nil {
		return "", err
	}

	files := extraFiles
	if requireRootForFiles && p.AllowedDir == "" {
		files = nil
	}
	if p.AllowedDir == "" && len(files) == 0 {
		// resolve_allowed_path returns the resolved path untouched when no
		// boundary is configured (workspace_policy.py:110-112).
		return resolved, nil
	}

	if p.AllowedDir != "" {
		if isPathWithin(resolved, p.AllowedDir) {
			return resolved, nil
		}
	}
	for _, dir := range extraDirs {
		if isPathWithin(resolved, dir) {
			return resolved, nil
		}
	}
	// _is_path_exactly_allowed requires the logical path to equal the resolved
	// path, so a symlink pointing at an allowed file is not granted
	// (workspace_policy.py:68-85).
	if len(files) > 0 && logical == resolved {
		for _, f := range files {
			if samePath(logical, f) {
				return resolved, nil
			}
		}
	}
	return "", &boundaryError{msg: fmt.Sprintf("Path %s is outside allowed directory %s", path, p.AllowedDir)}
}

// logicalPath mirrors _resolve_logical_path (workspace_policy.py:34-40): the
// path is made absolute without following symlinks.
func (p PathPolicy) logicalPath(path string) (string, error) {
	candidate := expandUser(path)
	if !filepath.IsAbs(candidate) && p.Workspace != "" {
		candidate = filepath.Join(p.Workspace, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// displayPath mirrors display_file_edit_path (utils/file_edit_events.py:154-160):
// the path relative to the workspace when it is inside, otherwise the absolute
// path. Separators are always forward slashes (Path.as_posix()).
func (p PathPolicy) displayPath(path string) string {
	if p.Workspace != "" {
		if rel, err := filepath.Rel(mustAbs(p.Workspace), path); err == nil {
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(path)
}

// expandUser mirrors Path.expanduser() for the "~" and "~/..." forms.
// UNVERIFIED: the "~user" form (Path.expanduser() supports it) is not
// implemented; such a path is treated literally.
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// resolveNonStrict mirrors Path.resolve(strict=False): symlinks are resolved for
// the longest existing prefix of the path and the non-existent remainder is
// appended verbatim.
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
			// Nothing on the path exists; keep the absolute logical form.
			return path, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// isPathWithin mirrors is_path_within (workspace_policy.py:46-56): both sides
// are resolved (non-strict) and the path must be the root or a descendant.
func isPathWithin(path, root string) bool {
	if root == "" {
		return false
	}
	p, err := resolveNonStrict(expandUser(path))
	if err != nil {
		return false
	}
	r, err := resolveNonStrict(expandUser(root))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// samePath compares two paths by their absolute, non-symlink-resolved form
// (_path_key, workspace_policy.py:42-43).
func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(expandUser(a))
	bb, err2 := filepath.Abs(expandUser(b))
	return err1 == nil && err2 == nil && aa == bb
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		return path
	}
	return abs
}

// ---------------------------------------------------------------------------
// Device paths and binary sniffing
// ---------------------------------------------------------------------------

// blockedDevicePaths mirrors _BLOCKED_DEVICE_PATHS (filesystem.py:202-207).
var blockedDevicePaths = map[string]bool{
	"/dev/zero":    true,
	"/dev/random":  true,
	"/dev/urandom": true,
	"/dev/full":    true,
	"/dev/stdin":   true,
	"/dev/stdout":  true,
	"/dev/stderr":  true,
	"/dev/tty":     true,
	"/dev/console": true,
	"/dev/fd/0":    true,
	"/dev/fd/1":    true,
	"/dev/fd/2":    true,
}

// procFDPattern merges the two reference regexes for descriptor paths
// (filesystem.py:223-226).
var procFDPattern = regexp.MustCompile(`^/proc/(?:[0-9]+|self)/fd/[012]$`)

// isBlockedDevice reports whether path is a device that could hang or produce
// unbounded output (filesystem.py:210-231).
func isBlockedDevice(path string) bool {
	raw := path
	resolved := raw
	if r, err := filepath.EvalSymlinks(raw); err == nil {
		resolved = r
	}
	if blockedDevicePaths[raw] || blockedDevicePaths[resolved] {
		return true
	}
	if procFDPattern.MatchString(raw) || procFDPattern.MatchString(resolved) {
		return true
	}
	return strings.HasPrefix(resolved, "/dev/")
}

// textExtensions mirrors _is_text_extension (nanobot/utils/document.py:627-643).
var textExtensions = map[string]bool{
	".txt":  true,
	".md":   true,
	".csv":  true,
	".json": true,
	".xml":  true,
	".html": true,
	".htm":  true,
	".log":  true,
	".yaml": true,
	".yml":  true,
	".toml": true,
	".ini":  true,
	".cfg":  true,
}

// detectImageMime mirrors detect_image_mime (nanobot/utils/helpers.py:327-336):
// PNG, JPEG, GIF87a/89a and RIFF+WEBP magic bytes.
func detectImageMime(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(raw, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		return "image/gif"
	case len(raw) >= 12 && bytes.HasPrefix(raw, []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}
