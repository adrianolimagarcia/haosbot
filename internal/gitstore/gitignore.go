package gitstore

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file ports the part of dulwich's ignore.py that decides whether
// porcelain.add stages a path.
//
// Why it matters: porcelain.add skips any path whose ignore check is truthy
// (dulwich/porcelain/__init__.py:1631 — "if ignore_manager.is_ignored(relpath):
// ignored.add(relpath); continue"). GitStore stages exactly the tracked files,
// so an ignore rule that wrongly matches one of them silently stops that file
// from ever being committed — a data-loss-shaped failure that no error surfaces.
// The .gitignore GitStore itself writes always re-includes the tracked files,
// but init merges that content into an existing .gitignore, and the tracked file
// list can grow afterwards, so the matcher has to be right.
//
// Scope: every source dulwich reads — .git/info/exclude, the user's global
// ignore file (core.excludesFile, else $XDG_CONFIG_HOME/git/ignore), and the
// per-directory .gitignore files from the worktree root downwards.
//
// The precedence is dulwich's, and it is NOT git's: matches are collected in
// filter order and the LAST match decides, where the order is
// [deepest .gitignore ... root .gitignore, info/exclude, global ignore]
// (IgnoreFilterManager.find_matching inserts each new directory filter at the
// front of the list and appends matches as it walks). Two consequences were
// verified against the reference:
//
//   - a global ignore file wins over .gitignore, so a global "*.md" stops
//     SOUL.md from being staged even though the generated .gitignore re-includes
//     it — auto_commit then silently commits nothing;
//   - an outer .gitignore wins over a nested one, so a nested
//     memory/.gitignore cannot un-track memory/MEMORY.md.
//
// core.ignorecase is honoured, but only for .gitignore files: IgnoreFilterManager
// builds the global filters with IgnoreFilter.from_path, which defaults the flag
// to false (dulwich/ignore.py:719-722), so info/exclude and the user ignore file
// stay case-sensitive whatever the configuration says. Verified against the
// reference: with core.ignorecase=true a ".gitignore" pattern "soul.md" hides
// SOUL.md, while the same pattern in info/exclude does not.

// ignorePattern is one compiled .gitignore line (dulwich/ignore.py:Pattern).
type ignorePattern struct {
	raw             string
	isExclude       bool // True => the pattern ignores matching paths
	isDirectoryOnly bool
	neverMatch      bool // pattern contains "//", which git treats as broken
	re              *regexp.Regexp
}

// match mirrors Pattern.match (dulwich/ignore.py).
func (p *ignorePattern) match(path string) bool {
	if p.neverMatch {
		return false
	}
	// Negation directory patterns (e.g. "!dir/") only ever match directories.
	if p.isDirectoryOnly && !p.isExclude && !strings.HasSuffix(path, "/") {
		return false
	}
	if p.reMatch(path) {
		return true
	}
	// An excluding directory pattern also matches files underneath it.
	if p.isDirectoryOnly && p.isExclude && !strings.HasSuffix(path, "/") && strings.Contains(path, "/") {
		idx := strings.LastIndex(path, "/")
		return p.reMatch(path[:idx+1])
	}
	return false
}

// reMatch is Python's re.match: the pattern must match at the start of the
// string (the compiled regex ends with \z, so it must match the whole string).
func (p *ignorePattern) reMatch(s string) bool {
	loc := p.re.FindStringIndex(s)
	return loc != nil && loc[0] == 0
}

// compileIgnorePattern builds a Pattern from one raw gitignore line.
func compileIgnorePattern(line string, ignoreCase bool) (*ignorePattern, bool) {
	p := &ignorePattern{raw: line}
	body := line
	if strings.HasPrefix(body, "!") {
		p.isExclude = false
		body = body[1:]
	} else {
		// A leading \! or \# escapes the special meaning of the first byte.
		if strings.HasPrefix(body, "\\") && len(body) > 1 && (body[1] == '!' || body[1] == '#') {
			body = body[1:]
		}
		p.isExclude = true
	}
	p.isDirectoryOnly = strings.HasSuffix(body, "/")
	if strings.Contains(body, "//") {
		// Git treats a pattern containing "//" as broken: it matches nothing.
		// Python spells that as a negative lookahead, which RE2 has no syntax
		// for, so the pattern is flagged instead.
		p.neverMatch = true
		return p, true
	}
	expr := translateIgnorePattern(body)
	if ignoreCase {
		// Python's re.IGNORECASE, applied by dulwich when core.ignorecase is set
		// (dulwich/ignore.py:348-350).
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		// Git ignores patterns it cannot compile; so does this port.
		return nil, false
	}
	p.re = re
	return p, true
}

// readIgnorePatterns mirrors read_ignore_patterns (dulwich/ignore.py).
func readIgnorePatterns(content string) []string {
	var out []string
	for _, raw := range splitLinesKeepEnds(content, true) {
		line := strings.TrimRight(raw, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Trailing spaces are ignored unless quoted with a backslash.
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
			line = line[:len(line)-1]
		}
		line = strings.ReplaceAll(line, "\\ ", " ")
		out = append(out, line)
	}
	return out
}

// ignoreFilter is dulwich's IgnoreFilter: an ordered list of patterns.
type ignoreFilter struct {
	patterns []*ignorePattern
}

func newIgnoreFilter(content string, ignoreCase bool) *ignoreFilter {
	f := &ignoreFilter{}
	for _, line := range readIgnorePatterns(content) {
		if p, ok := compileIgnorePattern(line, ignoreCase); ok {
			f.patterns = append(f.patterns, p)
		}
	}
	return f
}

func (f *ignoreFilter) findMatching(path string) []*ignorePattern {
	var out []*ignorePattern
	for _, p := range f.patterns {
		if p.match(path) {
			out = append(out, p)
		}
	}
	return out
}

// ignoreManager mirrors IgnoreFilterManager.
type ignoreManager struct {
	worktree   string
	globals    []*ignoreFilter          // info/exclude, then the user ignore file
	pathCache  map[string]*ignoreFilter // dirname -> its .gitignore, nil when absent
	ignoreCase bool
}

// newIgnoreManager builds the manager the way IgnoreFilterManager.from_repo does
// (dulwich/ignore.py:710): the global filters are .git/info/exclude and the
// user's ignore file, in that order.
func newIgnoreManager(gitDir, worktree string) (*ignoreManager, error) {
	ignoreCase, err := ignoreCaseConfig(gitDir)
	if err != nil {
		return nil, err
	}
	m := &ignoreManager{
		worktree:   worktree,
		pathCache:  map[string]*ignoreFilter{},
		ignoreCase: ignoreCase,
	}
	for _, path := range []string{
		filepath.Join(gitDir, "info", "exclude"),
		userIgnoreFilterPath(gitDir),
	} {
		if path == "" {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			// Case-sensitive, whatever core.ignorecase says: from_repo builds the
			// global filters without the flag.
			m.globals = append(m.globals, newIgnoreFilter(string(data), false))
		}
	}
	return m, nil
}

// userIgnoreFilterPath mirrors default_user_ignore_filter_path
// (dulwich/ignore.py:555): core.excludesFile from the configuration stack, else
// $XDG_CONFIG_HOME/git/ignore (~/.config/git/ignore). The path is expanded with
// os.path.expanduser.
func userIgnoreFilterPath(gitDir string) string {
	if v, ok := repoConfigValue(gitDir, "core", "excludesfile"); ok && v != "" {
		return expandUserPath(v)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "git", "ignore")
}

// expandUserPath mirrors os.path.expanduser for the leading "~" case.
func expandUserPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return p
}

// repoConfigValue reads a value from the repository configuration stack:
// .git/config first, then the user and system configuration files
// (dulwich/config.py:StackedConfig.get returns the first backend that has it).
func repoConfigValue(gitDir, section, name string) (string, bool) {
	for _, path := range append([]string{filepath.Join(gitDir, "config")}, gitConfigPaths()...) {
		if v, ok := readGitConfigValue(path, section, name); ok {
			return v, true
		}
	}
	return "", false
}

// ignoreCaseConfig mirrors the core.ignorecase lookup (dulwich/ignore.py:729),
// which defaults to false. dulwich parses it with get_boolean, which accepts
// only "true" and "false" (case-insensitively) and raises ValueError for
// anything else — a value such as "1" or "yes" therefore fails the whole
// auto_commit in the reference rather than being ignored, and the error below is
// what produces the same outcome here.
func ignoreCaseConfig(gitDir string) (bool, error) {
	v, ok := repoConfigValue(gitDir, "core", "ignorecase")
	if !ok {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("not a valid boolean string: %q", v)
	}
}

// loadPath mirrors IgnoreFilterManager._load_path: read
// <worktree>/<dirname>/.gitignore, remembering a missing file.
func (m *ignoreManager) loadPath(dirname string) *ignoreFilter {
	if f, ok := m.pathCache[dirname]; ok {
		return f
	}
	var f *ignoreFilter
	if data, err := os.ReadFile(filepath.Join(m.worktree, filepath.FromSlash(dirname), ".gitignore")); err == nil {
		f = newIgnoreFilter(string(data), m.ignoreCase)
	}
	m.pathCache[dirname] = f
	return f
}

// findMatching mirrors IgnoreFilterManager.find_matching exactly, including the
// order in which filters are consulted: the global filters start the list, and
// each directory's .gitignore is inserted at the front as the walk descends.
func (m *ignoreManager) findMatching(path string) []*ignorePattern {
	type scopedFilter struct {
		start  int
		filter *ignoreFilter
	}
	filters := make([]scopedFilter, 0, len(m.globals)+2)
	for _, f := range m.globals {
		filters = append(filters, scopedFilter{0, f})
	}

	parts := strings.Split(path, "/")
	var matches []*ignorePattern
	for i := 0; i <= len(parts); i++ {
		dirname := strings.Join(parts[:i], "/")
		for _, sf := range filters {
			relpath := strings.Join(parts[sf.start:i], "/")
			if i < len(parts) {
				// Everything before the final part is a directory, so the
				// trailing slash is part of the path being tested.
				relpath += "/"
			}
			matches = append(matches, sf.filter.findMatching(relpath)...)
		}
		if f := m.loadPath(dirname); f != nil {
			filters = append([]scopedFilter{{i, f}}, filters...)
		}
	}
	return matches
}

// isIgnored mirrors IgnoreFilterManager.is_ignored: nil when nothing matches,
// true when the path is ignored, false when a negation wins.
func (m *ignoreManager) isIgnored(path string) *bool {
	matches := m.findMatching(path)
	if len(matches) == 0 {
		return nil
	}
	result := matches[len(matches)-1].isExclude
	if !result {
		result = parentExclusionApplies(path, matches)
	}
	if result && strings.HasSuffix(path, "/") {
		result = m.applyDirectoryTraversalRule(path, matches)
	}
	return &result
}

// parentExclusionApplies mirrors _check_parent_exclusion (dulwich/ignore.py):
// git cannot re-include a file when one of its parent directories is excluded.
func parentExclusionApplies(path string, matches []*ignorePattern) bool {
	var finalNegation *ignorePattern
	for i := len(matches) - 1; i >= 0; i-- {
		if !matches[i].isExclude {
			finalNegation = matches[i]
			break
		}
	}
	if finalNegation == nil {
		return false
	}
	for _, p := range matches {
		if p.isExclude && patternExcludesParent(p.raw, path, finalNegation.raw) {
			return true
		}
	}
	return false
}

// patternExcludesParent mirrors _pattern_excludes_parent (dulwich/ignore.py).
func patternExcludesParent(patternStr, path, finalPatternStr string) bool {
	if strings.HasPrefix(patternStr, "**/") && strings.HasSuffix(patternStr, "/**") {
		middle := patternStr[3 : len(patternStr)-3]
		return strings.Contains("/"+path, "/"+middle+"/") || strings.HasPrefix(path, middle+"/")
	}
	if strings.HasSuffix(patternStr, "/**") && !strings.HasPrefix(patternStr, "**/") {
		baseDir := patternStr[:len(patternStr)-3]
		if !strings.HasPrefix(path, baseDir+"/") {
			return false
		}
		remaining := path[len(baseDir)+1:]
		// dir/** still allows negating an immediate child file.
		if !strings.HasSuffix(path, "/") && strings.HasPrefix(finalPatternStr, "!") && !strings.Contains(remaining, "/") {
			negPattern := finalPatternStr[1:]
			if negPattern == path || (strings.Contains(negPattern, "*") && !strings.Contains(negPattern, "**")) {
				return false
			}
		}
		if strings.Contains(finalPatternStr, "**") {
			if p, ok := compileIgnorePattern(strings.TrimPrefix(finalPatternStr, "!"), false); ok && p.match(path) {
				return false
			}
		}
		return true
	}
	// A directory pattern (trailing /) can exclude a parent directory.
	if strings.HasSuffix(patternStr, "/") && strings.Contains(path, "/") {
		p, ok := compileIgnorePattern(patternStr, false)
		if !ok {
			return false
		}
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			if p.match(strings.Join(parts[:i], "/") + "/") {
				return true
			}
		}
		return false
	}
	return false
}

// applyDirectoryTraversalRule mirrors _apply_directory_traversal_rule
// (dulwich/ignore.py, issue #1203): a directory matched by a ** pattern is still
// traversable when a subdirectory of it would be un-ignored.
func (m *ignoreManager) applyDirectoryTraversalRule(path string, matches []*ignorePattern) bool {
	var lastExcluding *ignorePattern
	for _, match := range matches {
		if match.isExclude {
			lastExcluding = match
		}
	}
	if lastExcluding != nil && strings.Contains(lastExcluding.raw, "**") {
		testMatches := m.findMatching(path + "test/")
		if len(testMatches) > 0 && !testMatches[len(testMatches)-1].isExclude {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// translate: gitignore pattern -> regular expression
// ---------------------------------------------------------------------------

// translateIgnorePattern mirrors translate() in dulwich/ignore.py, producing a
// Go regexp (RE2) instead of a Python one.
//
// The only deliberate difference is the end anchor: Python's "\Z" is spelled
// "\z" in Go. Callers match from the start of the string (reMatch), which is
// what Python's re.match does.
func translateIgnorePattern(pat string) string {
	res := "(?ms)"

	// Patterns without a slash (except a trailing one) match at any level.
	if !strings.Contains(pat[:max(0, len(pat)-1)], "/") {
		res += "(.*/)?"
	}

	pat, prefix := handleLeadingIgnorePatterns(pat)
	res += prefix

	if pat == "**" {
		res += ".*"
	} else {
		segments := strings.Split(pat, "/")
		for i := 0; i < len(segments); i++ {
			segment := segments[i]
			if i > 0 && segments[i-1] != "**" {
				res += "/"
			}
			if segment == "**" {
				regexPart, skipNext := handleDoubleAsterisk(segments, i)
				res += regexPart
				if regexPart == ".*" { // end of pattern
					break
				}
				if skipNext {
					i++
				}
			} else {
				res += translateIgnoreSegment(segment)
			}
		}
	}

	if !strings.HasSuffix(pat, "/") {
		res += "/?"
	}
	return res + `\z`
}

// handleLeadingIgnorePatterns mirrors _handle_leading_patterns (dulwich/ignore.py).
func handleLeadingIgnorePatterns(pat string) (string, string) {
	switch {
	case strings.HasPrefix(pat, "/**/"):
		return pat[4:], "(.*/)?"
	case strings.HasPrefix(pat, "**/"):
		return pat[3:], "(.*/)?"
	case strings.HasPrefix(pat, "/"):
		return pat[1:], ""
	default:
		return pat, ""
	}
}

// handleDoubleAsterisk mirrors _handle_double_asterisk (dulwich/ignore.py).
func handleDoubleAsterisk(segments []string, i int) (string, bool) {
	remaining := segments[i+1:]
	allEmpty := true
	for _, s := range remaining {
		if s != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		return ".*", false
	}
	if i+1 < len(segments) && segments[i+1] == "**" {
		afterNext := segments[i+2:]
		isDirPattern := len(afterNext) == 1 && afterNext[0] == ""
		if isDirPattern {
			return "[^/]+/(?:[^/]+/)*", true
		}
		return "(?:[^/]+/)*", true
	}
	if i == 0 {
		return "(?:.*/)??", false
	}
	return "(?:[^/]+/)*", false
}

// translateIgnoreSegment mirrors _translate_segment (dulwich/ignore.py).
func translateIgnoreSegment(segment string) string {
	if segment == "*" {
		return "[^/]+"
	}
	var res strings.Builder
	i := 0
	for i < len(segment) {
		c := segment[i]
		i++
		switch c {
		case '*':
			res.WriteString("[^/]*")
		case '?':
			res.WriteString("[^/]")
		case '\\':
			if i < len(segment) {
				res.WriteString(pyReEscapeByte(segment[i]))
				i++
			} else {
				res.WriteString(pyReEscapeByte(c))
			}
		case '[':
			j := i
			if j < len(segment) && segment[j] == '!' {
				j++
			}
			if j < len(segment) && segment[j] == ']' {
				j++
			}
			for j < len(segment) && segment[j] != ']' {
				j++
			}
			if j >= len(segment) {
				res.WriteString("\\[")
			} else {
				stuff := strings.ReplaceAll(segment[i:j], "\\", "\\\\")
				i = j + 1
				if strings.HasPrefix(stuff, "!") {
					stuff = "^" + stuff[1:]
				} else if strings.HasPrefix(stuff, "^") {
					stuff = "\\" + stuff
				}
				res.WriteString("[" + stuff + "]")
			}
		default:
			res.WriteString(pyReEscapeByte(c))
		}
	}
	return res.String()
}

// pyReEscapeByte mirrors Python's re.escape applied to a single byte: only the
// characters Python considers special are backslash-escaped.
func pyReEscapeByte(c byte) string {
	switch c {
	case '(', ')', '[', ']', '{', '}', '?', '*', '+', '-', '|', '^', '$', '\\', '.', '&', '~', '#', ' ', '\t', '\n', '\r', '\v', '\f':
		return "\\" + string(rune(c))
	}
	return string(rune(c))
}
