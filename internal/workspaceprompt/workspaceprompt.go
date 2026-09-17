// Package workspaceprompt ports nanobot/utils/workspace_prompts.py: the
// file-backed overrides that let a workspace replace a built-in prompt.
//
// Overrides live at <workspace>/prompts/<name>.md. A missing, unreadable,
// empty, or whitespace-only file is not an override — the caller falls back to
// its built-in default. That distinction is the whole contract, and it is why
// Load reports three values rather than returning a bare string: an override
// that is present but empty must be distinguishable from one that is absent.
package workspaceprompt

import (
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// MaxChars is WORKSPACE_PROMPT_MAX_CHARS (workspace_prompts.py:10). Overrides
// longer than this are truncated rather than rejected, so a runaway file
// degrades into a usable prompt instead of disabling the override.
const MaxChars = 32000

// File returns the conventional path for a named workspace prompt override,
// mirroring workspace_prompt_file. The name is used verbatim as the file stem.
func File(workspace, name string) string {
	return filepath.Join(workspace, "prompts", name+".md")
}

// Load reads a workspace prompt override and caps it at maxChars characters.
//
// It returns the text, the file's original character count BEFORE truncation,
// and whether a usable override exists. A maxChars <= 0 selects MaxChars;
// passing a non-positive value must not silently disable the cap, because the
// cap is the only thing standing between a pathological file and the model's
// context window.
//
// Mirrors load_workspace_prompt_override (workspace_prompts.py:18). The
// reference swallows OSError and UnicodeDecodeError and reports "no override";
// this reproduces that by treating any read failure and any invalid UTF-8 as
// absence, because Python's read_text raises before the text is ever seen.
func Load(path string, maxChars int) (text string, originalChars int, ok bool) {
	if maxChars <= 0 {
		maxChars = MaxChars
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, false
	}
	if !utf8.Valid(raw) {
		return "", 0, false
	}
	trimmed := textutil.PyRStrip(string(raw))
	if trimmed == "" {
		return "", 0, false
	}
	originalChars = utf8.RuneCountInString(trimmed)
	return textutil.TruncateText(trimmed, maxChars), originalChars, true
}

// Has reports whether path holds a non-empty override.
// Mirrors has_workspace_prompt_override (workspace_prompts.py:36).
func Has(path string) bool {
	_, _, ok := Load(path, MaxChars)
	return ok
}

// Initialize writes defaultPrompt to path when the target is missing or empty.
//
// It returns false without writing when the target already holds non-empty
// content, is not a regular file, or cannot be read safely. The guard is
// deliberately conservative: an unreadable path must not be overwritten,
// because "cannot read" and "safe to replace" are not the same condition.
//
// Mirrors initialize_workspace_prompt (workspace_prompts.py:42). The reference
// appends a single "\n" to the default, and that byte is part of the on-disk
// contract, so it is reproduced here.
func Initialize(path, defaultPrompt string) (bool, error) {
	// Any stat failure means Python's path.exists() would report False, so the
	// guard is skipped and the write is attempted. That includes EACCES and
	// ELOOP, not just ENOENT: Python proceeds in those cases and raises from
	// write_text, so the error must surface here too rather than being folded
	// into a silent "false".
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return false, nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, nil
		}
		if !utf8.Valid(raw) {
			return false, nil
		}
		if textutil.PyStrip(string(raw)) != "" {
			return false, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(defaultPrompt+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
