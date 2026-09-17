package skills

// pathlib.PurePath / Path semantics that the loader depends on.
//
// The reference builds every skill path with `/` on `Path` objects and then
// compares, joins and relativises them. Three of those operations do NOT behave
// like their os.path / filepath counterparts:
//
//   - `base / name` does not clean the result: Path("/a/b/../c") / "d" is
//     "/a/b/../c/d", whereas filepath.Join cleans to "/a/c/d".
//   - `str(path)` on POSIX is the path with "/" separators, and `.as_posix()`
//     is the same string.
//   - `relative_to(root)` is purely LEXICAL over the path's parts and raises
//     ValueError when the prefix does not match; it never touches the
//     filesystem and never normalises "..".
//
// The port therefore does not use filepath.Join for path ARITHMETIC that the
// reference performs with `/`.

import (
	"os"
	"path/filepath"
	"strings"
)

// pyPathParts splits a POSIX path into PurePath's `_parts` tuple.
//
//	"/a/b"  -> ["/", "a", "b"]
//	"a/b/"  -> ["a", "b"]
//	"/"     -> ["/"]
//	""      -> []
//
// Multiple consecutive separators collapse, matching PurePath's parser.
func pyPathParts(p string) []string {
	var parts []string
	absolute := strings.HasPrefix(p, "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		parts = append(parts, seg)
	}
	if absolute {
		return append([]string{"/"}, parts...)
	}
	return parts
}

// pyJoin mirrors `base / elem / ...` for path elements.
func pyJoin(base string, elems ...string) string {
	parts := pyPathParts(base)
	for _, elem := range elems {
		parts = append(parts, pyPathParts(elem)...)
	}
	if len(parts) == 0 {
		return ""
	}
	if parts[0] == "/" {
		return "/" + strings.Join(parts[1:], "/")
	}
	return strings.Join(parts, "/")
}

// pyRelativeTo ports `PurePath.relative_to(other)`.
//
// ok is false where Python raises ValueError. The reference does NOT guard that
// call in build_skills_summary (skills.py:260), so the failure mode is a
// propagating exception; see Loader.BuildSkillsSummary for how the port reports
// it instead of crashing.
func pyRelativeTo(path, root string) (string, bool) {
	self := pyPathParts(path)
	other := pyPathParts(root)
	if len(other) > len(self) {
		return "", false
	}
	for i, seg := range other {
		if self[i] != seg {
			return "", false
		}
	}
	rest := self[len(other):]
	return strings.Join(rest, "/"), true
}

// pyExpandUser ports Path.expanduser() for the "~" and "~/..." forms. A
// "~user" form that does not name the current user is returned unchanged, like
// Python when the user does not exist.
func pyExpandUser(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home := os.Getenv("HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return p
	}
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return home + p[1:]
	}
	return p
}

// pyResolve ports `Path.expanduser().resolve()` (non-strict): expand `~`, make
// the path absolute, and resolve symlinks in the components that exist.
//
// Go's filepath.EvalSymlinks fails outright on a missing path, so the longest
// existing prefix is resolved and the remainder appended — the same approach
// internal/gitstore/paths.go takes for the same reason. The residual difference
// from Python is a path containing ".." together with a symlinked component;
// Python's os.path.realpath resolves ".." after the links, filepath.Clean
// before them.
func pyResolve(p string) string {
	expanded := pyExpandUser(p)
	abs := expanded
	if !filepath.IsAbs(abs) {
		if wd, err := os.Getwd(); err == nil {
			abs = filepath.Join(wd, abs)
		}
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(abs)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, rest)
		}
		dir = parent
	}
}
