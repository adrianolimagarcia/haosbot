package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/filediff"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ApplyPatch applies a list of structured edits to one or more files.
//
// Ports ApplyPatchTool (nanobot/agent/tools/apply_patch.py:20-254). Mutating:
// embeds tools.Base, whose defaults are conservative.
//
// This tool is the reason read_file, write_file and edit_file all advertise
// "Prefer apply_patch ..." in their descriptions: the reference text was ported
// verbatim, and until now it pointed at a tool that did not exist here.
//
// Divergences (see doc.go):
//   - FileStates is not ported, so the reference's
//     `self._file_states.record_write(path)` bookkeeping
//     (apply_patch.py:246-247) is omitted. It only feeds read_file's
//     "[File unchanged since last read]" cache, which is also absent.
//   - OS error text is Go's, not Python's. A target that exists but is a
//     directory therefore reports a different (still error) message than the
//     reference's `[Errno 21] Is a directory: '...'`.
type ApplyPatch struct {
	tools.Base
	policy PathPolicy
	params json.RawMessage
}

// NewApplyPatch returns an apply_patch tool bound to policy.
func NewApplyPatch(policy PathPolicy) *ApplyPatch {
	return &ApplyPatch{
		policy: policy,
		// Mirrors the @tool_parameters decorator on ApplyPatchTool
		// (apply_patch.py:45-77). The inner edit object is deliberately NOT
		// strict: the reference's ObjectSchema leaves additional_properties
		// unset there, so only the root object carries
		// additionalProperties=false.
		params: objectSchema(
			[]string{"edits"},
			map[string]any{
				"edits": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": strProp("Path to the file to edit. Relative paths resolve against the " +
								"workspace; absolute paths and '..' obey the workspace access policy."),
							"action": map[string]any{
								"type":        "string",
								"description": "Operation type: replace or add.",
								"enum":        []string{"replace", "add"},
							},
							"old_text": nullableStrProp("Exact text to search for in the file. Required for replace."),
							"new_text": nullableStrProp("Text to replace with or append. Required for replace and add."),
						},
						"required": []string{"path", "action"},
					},
					"description": "List of edits to apply. Each edit specifies a file and the change to make.",
					"minItems":    1,
					"maxItems":    20,
				},
				"dry_run": boolProp("Validate and summarize the patch without writing files.", false),
			},
		),
	}
}

// Name mirrors ApplyPatchTool.name (apply_patch.py:82-84).
func (t *ApplyPatch) Name() string { return "apply_patch" }

// Description mirrors ApplyPatchTool.description (apply_patch.py:86-95) verbatim.
func (t *ApplyPatch) Description() string {
	return "Default tool for code edits. Supports multi-file changes in a single call. " +
		"Provide a list of structured edits, each specifying a file path, action " +
		"(replace/add), and the exact text to change. " +
		"Paths are resolved by the current workspace access policy. " +
		"Set dry_run=true to validate and preview without writing files. " +
		"Use edit_file only for small exact replacements on a single file."
}

// Parameters returns the JSON Schema for the arguments (apply_patch.py:45-77).
func (t *ApplyPatch) Parameters() json.RawMessage { return t.params }

// patchError mirrors _PatchError (apply_patch.py:20-21).
type patchError struct{ msg string }

func (e *patchError) Error() string { return e.msg }

func patchErrf(format string, a ...any) *patchError {
	return &patchError{msg: fmt.Sprintf(format, a...)}
}

// Execute ports ApplyPatchTool.execute (apply_patch.py:97-254).
func (t *ApplyPatch) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	args, bad := parseArgs(t.Name(), raw, "edits", "dry_run")
	if bad != nil {
		return *bad, nil
	}
	dryRun, _, bad := optionalBool(t.Name(), args, "dry_run")
	if bad != nil {
		return *bad, nil
	}

	res, err := t.apply(args["edits"], dryRun)
	if err != nil {
		return patchErrorResult(err), nil
	}
	return res, nil
}

// patchErrorResult mirrors the three except arms of ApplyPatchTool.execute
// (apply_patch.py:249-254).
//
// The PermissionError arm comes first because that is the reference's order.
// A workspace boundary violation is a PermissionError subclass upstream
// (security/workspace_policy.py:24), so boundaryError is routed here too and
// produces `Error: <message>` rather than `Error applying patch: <message>`.
func patchErrorResult(err error) tools.Result {
	var boundary *boundaryError
	var pe *patchError
	switch {
	case errors.As(err, &boundary), errors.Is(err, fs.ErrPermission):
		return tools.Errf("Error: %v", err)
	case errors.As(err, &pe):
		return tools.Errf("Error applying patch: %v", pe)
	default:
		return tools.Errf("Error applying patch: %v", err)
	}
}

// apply is the body of ApplyPatchTool.execute inside its try block.
func (t *ApplyPatch) apply(editsValue any, dryRun bool) (tools.Result, error) {
	if !pyTruthy(editsValue) {
		return tools.Result{}, patchErrf("must provide edits")
	}
	elems, iterable := pyIterate(editsValue)
	if !iterable {
		// Python raises TypeError inside the loop header; it is caught by the
		// generic arm and reported as text. CPython quotes the type name:
		// "TypeError: 'int' object is not iterable".
		return tools.Result{}, fmt.Errorf("'%s' object is not iterable", pyTypeName(editsValue))
	}

	writes := newPatchWrites()
	originals := make(map[string]string)
	actions := make(map[string]string)

	for _, editValue := range elems {
		edit, ok := editValue.(map[string]any)
		if !ok {
			return tools.Result{}, patchErrf("each edit must be an object")
		}
		rawPath, ok := edit["path"].(string)
		if !ok {
			return tools.Result{}, patchErrf("path required for edit")
		}
		path, err := validatePatchPath(rawPath)
		if err != nil {
			return tools.Result{}, err
		}
		action, ok := edit["action"].(string)
		if !ok {
			return tools.Result{}, patchErrf("action required for edit: %s", path)
		}
		source, err := t.policy.resolveWrite(path)
		if err != nil {
			return tools.Result{}, err
		}

		var content, actionName string
		switch action {
		case "add":
			content, actionName, err = t.applyAdd(edit, path, source, writes)
		case "replace":
			content, actionName, err = t.applyReplace(edit, path, source, writes)
		default:
			return tools.Result{}, patchErrf("unknown action: %s", action)
		}
		if err != nil {
			return tools.Result{}, err
		}

		// setdefault: the FIRST original text and the FIRST action name win,
		// which is what the diff of a file edited twice must be computed
		// against.
		if _, seen := originals[source]; !seen {
			originals[source] = content
		}
		if _, seen := actions[source]; !seen {
			actions[source] = actionName
		}
	}

	diffs := make(map[string]filediff.FileDiff, len(writes.order))
	for _, source := range writes.order {
		diffs[source] = filediff.FileDiffFromText(originals[source], writes.data[source])
	}

	summaries := make([]string, 0, len(writes.order))
	for _, source := range writes.order {
		diff := diffs[source]
		stats := ""
		if diff.Added != 0 || diff.Deleted != 0 {
			stats = fmt.Sprintf(" (+%d/-%d)", diff.Added, diff.Deleted)
		}
		summaries = append(summaries, fmt.Sprintf("- %s %s%s",
			actions[source], t.policy.displayPath(source), stats))
	}

	if dryRun {
		return tools.OK("Patch dry-run succeeded:\n" + strings.Join(summaries, "\n")), nil
	}

	// Snapshot every target BEFORE writing anything, so a failure part way
	// through can restore the workspace exactly (apply_patch.py:228-244).
	backups := make(map[string][]byte, len(writes.order))
	missing := make(map[string]bool, len(writes.order))
	for _, path := range writes.order {
		if _, statErr := os.Stat(path); statErr != nil {
			missing[path] = true
			continue
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return tools.Result{}, readErr
		}
		backups[path] = data
	}

	if writeErr := writes.writeAll(); writeErr != nil {
		// Rollback. A failure here replaces the original error, exactly as it
		// does upstream, where the except block's own exception propagates.
		for _, path := range writes.order {
			if data, ok := backups[path]; ok {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return tools.Result{}, err
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					return tools.Result{}, err
				}
				continue
			}
			if !missing[path] {
				continue
			}
			if _, statErr := os.Stat(path); statErr == nil {
				if err := os.Remove(path); err != nil {
					return tools.Result{}, err
				}
			}
		}
		return tools.Result{}, writeErr
	}

	return tools.Result{
		Content:   "Patch applied:\n" + strings.Join(summaries, "\n"),
		FileDiffs: diffs,
	}, nil
}

// applyAdd ports the "add" arm (apply_patch.py:124-157). It returns the
// pre-edit content of the file (the diff's left-hand side).
func (t *ApplyPatch) applyAdd(
	edit map[string]any, path, source string, writes *patchWrites,
) (content, actionName string, err error) {
	newTextValue, present := edit["new_text"]
	if !present || newTextValue == nil {
		return "", "", patchErrf("new_text required for add: %s", path)
	}

	pending, hasPending := writes.get(source)
	var exists bool
	switch {
	case hasPending:
		content, exists = pending, true
	case fileExists(source):
		raw, readErr := os.ReadFile(source)
		if readErr != nil {
			return "", "", readErr
		}
		if !utf8.Valid(raw) {
			return "", "", patchErrf("file is not UTF-8 text: %s", path)
		}
		content, exists = string(raw), true
	default:
		content, exists = "", false
	}

	// The reference does not type-check new_text; a non-str reaches .replace()
	// and the AttributeError is reported as text.
	newText, isStr := newTextValue.(string)
	if !isStr {
		return "", "", pyAttributeError("replace", newTextValue)
	}

	if exists {
		usesCRLF := strings.Contains(content, "\r\n")
		newNorm := appendText(content, newText)
		if usesCRLF {
			newNorm = strings.ReplaceAll(newNorm, "\n", "\r\n")
		}
		writes.set(source, newNorm)
		return content, "update", nil
	}

	// Adding a NEW file does not restore CRLF: the text is normalised to \n and
	// only gains a trailing newline. This asymmetry with the branch above is
	// the reference's, not a porting slip.
	newNorm := strings.ReplaceAll(newText, "\r\n", "\n")
	if newNorm != "" && !strings.HasSuffix(newNorm, "\n") {
		newNorm += "\n"
	}
	writes.set(source, newNorm)
	return content, "add", nil
}

// applyReplace ports the "replace" arm (apply_patch.py:159-205).
func (t *ApplyPatch) applyReplace(
	edit map[string]any, path, source string, writes *patchWrites,
) (content, actionName string, err error) {
	oldTextValue := edit["old_text"]
	// `edit.get("old_text") or ""` — any falsy value becomes the empty string
	// and is then rejected.
	if !pyTruthy(oldTextValue) {
		return "", "", patchErrf("old_text required for replace: %s", path)
	}
	newTextValue, present := edit["new_text"]
	if !present || newTextValue == nil {
		return "", "", patchErrf("new_text required for replace: %s", path)
	}

	pending, hasPending := writes.get(source)
	switch {
	case hasPending:
		content = pending
	case fileExists(source):
		raw, readErr := os.ReadFile(source)
		if readErr != nil {
			return "", "", readErr
		}
		if !utf8.Valid(raw) {
			return "", "", patchErrf("file is not UTF-8 text: %s", path)
		}
		content = string(raw)
	default:
		return "", "", patchErrf("file to update does not exist: %s", path)
	}
	if !hasPending && !isRegularFile(source) {
		return "", "", patchErrf("path to update is not a file: %s", path)
	}

	oldText, isStr := oldTextValue.(string)
	if !isStr {
		return "", "", pyAttributeError("replace", oldTextValue)
	}

	usesCRLF := strings.Contains(content, "\r\n")
	normContent := strings.ReplaceAll(content, "\r\n", "\n")
	normOld := strings.ReplaceAll(oldText, "\r\n", "\n")

	pos := strings.Index(normContent, normOld)
	if pos < 0 {
		return "", "", patchErrf("old_text not found in %s", path)
	}
	// The reference searches from pos+1 (one CHARACTER), not from the end of
	// the match, so overlapping occurrences still count as ambiguous. pos+1 is
	// a byte offset here; a match can only begin on a character boundary, and a
	// multi-byte first character cannot re-occur inside itself, so the byte and
	// character searches find a second occurrence under exactly the same
	// conditions.
	if strings.Contains(normContent[pos+1:], normOld) {
		return "", "", patchErrf("old_text appears multiple times in %s", path)
	}

	newText, isStr := newTextValue.(string)
	if !isStr {
		return "", "", pyAttributeError("replace", newTextValue)
	}

	newNorm := normContent[:pos] + strings.ReplaceAll(newText, "\r\n", "\n") + normContent[pos+len(normOld):]
	if newNorm != "" && !strings.HasSuffix(newNorm, "\n") {
		newNorm += "\n"
	}
	if usesCRLF {
		newNorm = strings.ReplaceAll(newNorm, "\n", "\r\n")
	}
	writes.set(source, newNorm)
	return content, "update", nil
}

// validatePatchPath ports _validate_patch_path (apply_patch.py:24-30).
//
// The strip is Python's, not Go's: NBSP and U+001C..U+001F are whitespace for
// str.strip() and are not for strings.TrimSpace.
func validatePatchPath(path string) (string, error) {
	normalized := textutil.PyStrip(path)
	if normalized == "" {
		return "", patchErrf("patch path cannot be empty")
	}
	if strings.ContainsRune(normalized, 0) {
		return "", patchErrf("patch path contains a null byte: %s", pyReprStr(path))
	}
	return normalized, nil
}

// appendText ports _append_text (apply_patch.py:33-42): append without merging
// the addition into an unterminated final line.
func appendText(content, addition string) string {
	base := strings.ReplaceAll(content, "\r\n", "\n")
	extra := strings.ReplaceAll(addition, "\r\n", "\n")
	if base != "" && extra != "" && !strings.HasSuffix(base, "\n") && !strings.HasPrefix(extra, "\n") {
		base += "\n"
	}
	combined := base + extra
	if combined != "" && !strings.HasSuffix(combined, "\n") {
		combined += "\n"
	}
	return combined
}

// fileExists mirrors Path.exists(), which reports False for a broken symlink
// and True for a directory.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isRegularFile mirrors Path.is_file().
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// patchWrites is the reference's `writes` dict (apply_patch.py:107): a mapping
// from resolved path to pending content that preserves insertion order, because
// the write loop and the summary list both follow it. Go maps do not, so the
// order is carried alongside.
type patchWrites struct {
	order []string
	data  map[string]string
}

func newPatchWrites() *patchWrites {
	return &patchWrites{data: make(map[string]string)}
}

func (w *patchWrites) get(key string) (string, bool) {
	v, ok := w.data[key]
	return v, ok
}

func (w *patchWrites) set(key, value string) {
	if _, ok := w.data[key]; !ok {
		w.order = append(w.order, key)
	}
	w.data[key] = value
}

// writeAll performs the transactional write pass (apply_patch.py:232-235).
func (w *patchWrites) writeAll() error {
	for _, path := range w.order {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(w.data[path]), 0o644); err != nil {
			return err
		}
	}
	return nil
}
