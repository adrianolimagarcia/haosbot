package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/filediff"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// maxEditFileSize is the largest file edit_file will load.
//
// The reference allows 1 GiB (EditFileTool._MAX_EDIT_FILE_SIZE,
// nanobot/agent/tools/filesystem.py:864); this port lowers the cap to 64 MiB
// because the whole file must be held in memory (twice) to perform the
// replacement. The error message states the effective cap rather than the
// reference's.
const maxEditFileSize = 64 * 1024 * 1024

// markdownExtensions mirrors EditFileTool._MARKDOWN_EXTS (filesystem.py:865):
// trailing whitespace is preserved there because two trailing spaces are a hard
// line break in Markdown.
var markdownExtensions = map[string]bool{".md": true, ".mdx": true, ".markdown": true}

// EditFile performs an exact string replacement in one file.
//
// Ports EditFileTool (nanobot/agent/tools/filesystem.py:831-1040). Mutating:
// embeds tools.Base.
//
// Divergences (see doc.go for the full list):
//   - Only exact matching is ported. The looser matchers
//     (filesystem.py:776-787) would guess at the intended span; a near-miss is
//     reported as not-found instead.
//   - An ambiguous old_text is an error result rather than the reference's
//     non-error "Warning: ..." string (filesystem.py:974-978), so the model
//     must observe and resolve the ambiguity.
type EditFile struct {
	tools.Base
	policy PathPolicy
	params json.RawMessage
}

// NewEditFile returns an edit_file tool bound to policy.
func NewEditFile(policy PathPolicy) *EditFile {
	return &EditFile{
		policy: policy,
		// Mirrors the @tool_parameters decorator on EditFileTool
		// (filesystem.py:831-858).
		params: objectSchema(
			[]string{"path", "old_text", "new_text"},
			map[string]any{
				"path":        strProp("The file path to edit"),
				"old_text":    strProp("The text to find and replace; copy it from read_file."),
				"new_text":    strProp("The replacement text; must differ from old_text for an existing file."),
				"replace_all": plainBoolProp("Replace all occurrences (default false)"),
				"occurrence": nullableMinIntProp(
					"Optional 1-based occurrence to replace when old_text appears multiple times.", 1),
				"line_hint": nullableMinIntProp(
					"Optional exact 1-based target line copied from read_file. "+
						"The selected old_text match must cover this line.", 1),
				"expected_replacements": nullableMinIntProp(
					"Optional guard for the number of replacements that must be made.", 1),
			},
		),
	}
}

// Name mirrors EditFileTool.name (filesystem.py:867-869).
func (t *EditFile) Name() string { return "edit_file" }

// Description mirrors EditFileTool.description (filesystem.py:871-877) verbatim.
func (t *EditFile) Description() string {
	return "Perform a small, exact replacement in one file. " +
		"Prefer apply_patch for multi-file, structural, or generated edits. " +
		"occurrence, line_hint, and replace_all=true are mutually exclusive."
}

// Parameters returns the JSON Schema for the arguments (filesystem.py:831-858).
func (t *EditFile) Parameters() json.RawMessage { return t.params }

// matchSpan mirrors _MatchSpan (filesystem.py:671-677).
type matchSpan struct {
	start int
	end   int
	text  string
	line  int
}

// endLine mirrors _match_end_line (filesystem.py:679-682).
func (m matchSpan) endLine() int {
	comparable := m.text
	if strings.HasSuffix(comparable, "\n") {
		comparable = comparable[:len(comparable)-1]
	}
	return m.line + strings.Count(comparable, "\n")
}

// findExactMatches mirrors _find_exact_matches (filesystem.py:688-704).
func findExactMatches(content, oldText string) []matchSpan {
	var matches []matchSpan
	start := 0
	for {
		idx := strings.Index(content[start:], oldText)
		if idx < 0 {
			break
		}
		abs := start + idx
		matches = append(matches, matchSpan{
			start: abs,
			end:   abs + len(oldText),
			text:  oldText,
			line:  strings.Count(content[:abs], "\n") + 1,
		})
		start = abs + max(1, len(oldText))
	}
	return matches
}

// Execute ports EditFileTool.execute (filesystem.py:896-1040).
func (t *EditFile) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	args, bad := parseArgs(t.Name(), raw,
		"path", "old_text", "new_text", "replace_all", "occurrence", "line_hint", "expected_replacements")
	if bad != nil {
		return *bad, nil
	}
	path, bad := requiredString(t.Name(), args, "path")
	if bad != nil {
		return *bad, nil
	}
	oldText, bad := requiredString(t.Name(), args, "old_text")
	if bad != nil {
		return *bad, nil
	}
	newText, bad := requiredString(t.Name(), args, "new_text")
	if bad != nil {
		return *bad, nil
	}
	replaceAll, _, bad := optionalBool(t.Name(), args, "replace_all")
	if bad != nil {
		return *bad, nil
	}
	occurrence, hasOccurrence, bad := optionalInt(t.Name(), args, "occurrence")
	if bad != nil {
		return *bad, nil
	}
	lineHint, hasLineHint, bad := optionalInt(t.Name(), args, "line_hint")
	if bad != nil {
		return *bad, nil
	}
	expected, hasExpected, bad := optionalInt(t.Name(), args, "expected_replacements")
	if bad != nil {
		return *bad, nil
	}

	if path == "" {
		return tools.Errf("Error editing file: Unknown path"), nil
	}
	if hasOccurrence && occurrence < 1 {
		return tools.Errf("Error: occurrence must be >= 1."), nil
	}
	if hasLineHint && lineHint < 1 {
		return tools.Errf("Error: line_hint must be >= 1."), nil
	}
	if hasExpected && expected < 1 {
		return tools.Errf("Error: expected_replacements must be >= 1."), nil
	}

	fp, err := t.policy.resolveWrite(path)
	if err != nil {
		return permissionOrGenericEdit(err), nil
	}

	// Path.exists() swallows OSError, so any stat failure counts as "missing".
	info, statErr := os.Stat(fp)
	fileExists := statErr == nil
	if fileExists && oldText == newText {
		return tools.Errf("Error: new_text must be different from old_text."), nil
	}

	// Create-file semantics: old_text="" plus a missing file creates it
	// (filesystem.py:921-928).
	if !fileExists {
		if oldText == "" {
			if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
				return permissionOrGenericEdit(err), nil
			}
			if err := os.WriteFile(fp, []byte(newText), 0o644); err != nil {
				return permissionOrGenericEdit(err), nil
			}
			// The reference re-reads the file here
			// (filesystem.py:926); the bytes just written are the same
			// value, because the port writes newText verbatim.
			return t.summary(fp, "", newText, true), nil
		}
		// NOTE: the reference appends "Did you mean ...?" sibling suggestions
		// (filesystem.py:1042-1053, difflib.get_close_matches); not ported.
		return tools.Errf("Error: File not found: %s", path), nil
	}
	if info.Size() > maxEditFileSize {
		return tools.Errf("Error: File too large to edit (%.1f MiB). Maximum is %d MiB.",
			float64(info.Size())/(1024*1024), maxEditFileSize/(1024*1024)), nil
	}

	// Create-file: old_text="" on an existing, empty file overwrites it
	// (filesystem.py:938-946).
	if oldText == "" {
		existing, err := os.ReadFile(fp)
		if err != nil {
			return permissionOrGenericEdit(err), nil
		}
		if !utf8.Valid(existing) {
			return tools.Errf("Error editing file: %s is not valid UTF-8.", path), nil
		}
		if strings.TrimSpace(string(existing)) != "" {
			return tools.Errf("Error: Cannot create file — %s already exists and is not empty.", path), nil
		}
		if err := os.WriteFile(fp, []byte(newText), 0o644); err != nil {
			return permissionOrGenericEdit(err), nil
		}
		return t.summary(fp, string(existing), newText, false), nil
	}

	rawBytes, err := os.ReadFile(fp)
	if err != nil {
		return permissionOrGenericEdit(err), nil
	}
	if !utf8.Valid(rawBytes) {
		// The reference raises UnicodeDecodeError here and reports it through the
		// generic handler (filesystem.py:1039-1040).
		return tools.Errf("Error editing file: %s is not valid UTF-8.", path), nil
	}
	usesCRLF := bytes.Contains(rawBytes, []byte("\r\n"))
	content := strings.ReplaceAll(string(rawBytes), "\r\n", "\n")
	normOld := strings.ReplaceAll(oldText, "\r\n", "\n")
	matches := findExactMatches(content, normOld)

	if len(matches) == 0 {
		// The reference's near-match diff and cause hints (filesystem.py:1056-1079)
		// need difflib.SequenceMatcher and are not ported, so the final fallback
		// message is always used.
		return tools.Errf("Error: old_text not found in %s. No similar text found. Verify the file content.", path), nil
	}
	count := len(matches)
	if replaceAll && hasOccurrence {
		return tools.Errf("Error: occurrence cannot be used with replace_all=true."), nil
	}
	if replaceAll && hasLineHint {
		return tools.Errf("Error: line_hint cannot be used with replace_all=true."), nil
	}
	if hasOccurrence && hasLineHint {
		return tools.Errf("Error: line_hint cannot be used with occurrence."), nil
	}
	if hasOccurrence && occurrence > count {
		return tools.Errf("Error: occurrence %d is out of range; old_text appears %d time(s).", occurrence, count), nil
	}
	if count > 1 && !replaceAll && !hasOccurrence && !hasLineHint {
		preview := make([]string, 0, 3)
		for i, m := range matches {
			if i == 3 {
				break
			}
			preview = append(preview, fmt.Sprintf("line %d", m.line))
		}
		locationHint := ""
		if len(preview) > 0 {
			locationHint = " at " + strings.Join(preview, ", ")
			if count > 3 {
				locationHint += ", ..."
			}
		}
		return tools.Errf("Warning: old_text appears %d times%s. "+
			"Provide more context, set occurrence to choose one match, "+
			"or set replace_all=true.", count, locationHint), nil
	}

	normNew := strings.ReplaceAll(newText, "\r\n", "\n")
	if !markdownExtensions[strings.ToLower(filepath.Ext(fp))] {
		normNew = stripTrailingWS(normNew)
	}

	var selected []matchSpan
	switch {
	case replaceAll:
		selected = matches
	case hasOccurrence:
		selected = matches[occurrence-1 : occurrence]
	case hasLineHint:
		var candidates []matchSpan
		for _, m := range matches {
			if m.line <= lineHint && lineHint <= m.endLine() {
				candidates = append(candidates, m)
			}
		}
		if len(candidates) == 0 {
			locations := make([]string, 0, 3)
			for i, m := range matches {
				if i == 3 {
					break
				}
				locations = append(locations, fmt.Sprintf("line %d", m.line))
			}
			if count > 3 {
				locations = append(locations, "...")
			}
			return tools.Errf("Error: line_hint %d does not match the old_text location. "+
				"old_text appears at %s. Re-read the intended region and "+
				"copy old_text that covers the target line.", lineHint, strings.Join(locations, ", ")), nil
		}
		if len(candidates) > 1 {
			return tools.Errf("Error: line_hint %d is ambiguous; old_text appears %d times on that line.",
				lineHint, len(candidates)), nil
		}
		selected = candidates
	default:
		selected = matches[:1]
	}

	if hasExpected && len(selected) != expected {
		return tools.Errf("Error: expected %d replacements but would make %d.", expected, len(selected)), nil
	}

	// The reference splices the selected spans in reverse order
	// (filesystem.py:1014-1030). The matches are non-overlapping and sorted, so
	// a single forward pass is equivalent.
	//
	// _preserve_quote_style and _reindent_like_match (filesystem.py:618-668,
	// :1016-1017) are identity transforms when the matched text equals old_text,
	// which is always the case under exact matching, so they are omitted.
	var out strings.Builder
	out.Grow(len(content))
	prev := 0
	for _, m := range selected {
		end := m.end
		// Only consume the trailing newline when deleting a complete line.
		if normNew == "" &&
			(m.start == 0 || content[m.start-1] == '\n') &&
			!strings.HasSuffix(m.text, "\n") &&
			end < len(content) && content[end] == '\n' {
			end++
		}
		out.WriteString(content[prev:m.start])
		out.WriteString(normNew)
		prev = end
	}
	out.WriteString(content[prev:])
	newContent := out.String()
	if usesCRLF {
		newContent = strings.ReplaceAll(newContent, "\n", "\r\n")
	}
	if err := os.WriteFile(fp, []byte(newContent), 0o644); err != nil {
		return permissionOrGenericEdit(err), nil
	}
	return t.summary(fp, content, newContent, false), nil
}

// summary ports EditFileTool._format_summary (filesystem.py:884-894),
// including the FileDiff "(+N/-M)" suffix and the diffs the reference attaches
// to the returned FileEditResult.
//
// before/after are the same pair the reference passes at each call site: the
// pre-edit content (empty for a created file) and the bytes actually written.
// FileDiff.from_text normalises CRLF itself, so passing the CRLF-restored
// `after` is what the reference does and gives the same line counts.
func (t *EditFile) summary(fp, before, after string, created bool) tools.Result {
	action := "update"
	if created {
		action = "add"
	}
	diff := filediff.FileDiffFromText(before, after)
	stats := ""
	if diff.Added != 0 || diff.Deleted != 0 {
		stats = fmt.Sprintf(" (+%d/-%d)", diff.Added, diff.Deleted)
	}
	return tools.Result{
		Content: fmt.Sprintf("Patch applied:\n- %s %s%s", action, t.policy.displayPath(fp), stats),
		FileDiffs: map[string]filediff.FileDiff{
			fp: diff,
		},
	}
}

// stripTrailingWS mirrors EditFileTool._strip_trailing_ws (filesystem.py:879-882).
//
// Divergence: Python's str.rstrip() also treats U+001C-U+001F as whitespace;
// unicode.IsSpace does not.
func stripTrailingWS(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return strings.Join(lines, "\n")
}

// permissionOrGenericEdit mirrors the reference's PermissionError / generic
// error handling (filesystem.py:1037-1040).
func permissionOrGenericEdit(err error) tools.Result {
	// WorkspaceBoundaryError is a PermissionError subclass upstream
	// (security/workspace_policy.py:24), so a policy escape must take the
	// reference's `except PermissionError` arm at filesystem.py:1037-1038 and
	// render as "Error: ...". Without this, a boundary escape fell through to
	// the generic arm and was reported as "Error editing file: ...".
	var boundary *boundaryError
	if errors.As(err, &boundary) || errors.Is(err, fs.ErrPermission) {
		return tools.Errf("Error: %v", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return tools.Errf("Error: File not found: %v", err)
	}
	return tools.Errf("Error editing file: %v", err)
}
