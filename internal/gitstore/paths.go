package gitstore

import (
	"os"
	"path/filepath"
	"strings"
)

// pathlib-compatible path handling.
//
// The reference builds every filesystem path with pathlib (`self._workspace /
// path`, gitstore.py:102, :172, :349), and two of pathlib's rules are observable:
//
//   - Joining an absolute path DISCARDS the base. `Path("/ws") / "/etc/passwd"`
//     is `/etc/passwd`, so summarize_working_tree(["<absolute>"]) reads that
//     file and reports it in the summary. The reference does not guard against
//     this; this port reproduces it because the summary text is compared
//     elsewhere. (Upstream callers only ever pass workspace-relative paths.)
//   - Path.parts drops "." components and empty components, which is why
//     summarize_working_tree(["./SOUL.md"]) reads the HEAD blob for "SOUL.md"
//     while labelling the summary line "./SOUL.md" (gitstore.py:341-382, :516).
//
// Absolute paths are also produced by Path.absolute() in _staging_paths
// (gitstore.py:170-172), which normalises lexically but does NOT resolve
// symlinks — the opposite of Path.resolve(), which porcelain.add then applies.

// pythonPathJoin mirrors pathlib's "/" operator.
func pythonPathJoin(base, p string) string {
	if p == "" {
		return base
	}
	if filepath.IsAbs(p) {
		return p
	}
	if base == "" {
		return p
	}
	return strings.TrimRight(base, "/") + "/" + p
}

// pythonPathParts mirrors pathlib.PurePath.parts for a POSIX path: a leading "/"
// becomes its own part, and "." and empty components are dropped. ".." is kept.
func pythonPathParts(p string) []string {
	var parts []string
	if strings.HasPrefix(p, "/") {
		parts = append(parts, "/")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

// pythonAbsolute mirrors Path.absolute(): make the path absolute and normalise it
// lexically, without touching symlinks.
func pythonAbsolute(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(wd, p))
}

// resolveNonStrict mirrors Path.resolve() (non-strict since Python 3.6): resolve
// symlinks in the components that exist and leave the rest alone. Go's
// filepath.EvalSymlinks fails on a missing path, so the longest existing prefix
// is resolved and the remainder appended.
//
// One caveat: pythonAbsolute cleans the path lexically before symlinks are
// resolved, while Path.resolve() walks ".." after resolving each component. The
// two differ only for a path that contains ".." *and* a symlinked directory, and
// GitStore's API cannot produce one — every path it builds is a workspace-
// relative file name joined to the workspace root.
func resolveNonStrict(p string) string {
	abs := pythonAbsolute(p)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, rest)
		}
		dir = parent
	}
}
