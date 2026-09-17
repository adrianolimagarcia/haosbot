package agent

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// Tool-result normalization and offloading.
//
// Ports ContextGovernor.normalize_tool_result (context_governance.py:709),
// maybe_persist_tool_result (helpers.py:580) and ensure_nonempty_tool_result
// (runtime.py:48).
//
// Two problems are solved here, both of which corrupt a session in ways that
// are hard to attribute afterwards:
//
//  1. An empty tool result is ambiguous. A model that receives "" cannot tell
//     "the tool ran and produced nothing" from "the tool result was lost", and
//     tends to retry the call in a loop. A short marker removes the ambiguity.
//
//  2. An oversized result is sent in full, consuming the context window and
//     pushing the session toward compaction. The reference instead writes the
//     payload into the workspace and sends a bounded reference, so the model
//     can still read the whole output when it actually needs it.

// Constants from helpers.py:366-370.
const (
	// toolResultsDir is _TOOL_RESULTS_DIR.
	toolResultsDir = ".nanobot/tool-results"
	// toolResultPreviewChars is _TOOL_RESULT_PREVIEW_CHARS.
	toolResultPreviewChars = 1200
	// toolResultRetentionSecs is _TOOL_RESULT_RETENTION_SECS (7 days).
	toolResultRetentionSecs = 7 * 24 * 60 * 60
	// toolResultMaxBuckets is _TOOL_RESULT_MAX_BUCKETS.
	toolResultMaxBuckets = 32
)

// unsafeChars mirrors _UNSAFE_CHARS (helpers.py:366).
var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*]`)

// toolResultOffloadExempt mirrors TOOL_RESULT_OFFLOAD_EXEMPT_TOOLS
// (context_governance.py:69). read_file has its own bound, so offloading it
// would create a persist -> read -> persist loop.
var toolResultOffloadExempt = map[string]bool{"read_file": true}

// EmptyToolResultMessage mirrors empty_tool_result_message (runtime.py:43).
func EmptyToolResultMessage(toolName string) string {
	return "(" + toolName + " completed with no output)"
}

// SafeFilename mirrors safe_filename (helpers.py:374).
//
// The reference ends in a Python .strip() (`_UNSAFE_CHARS.sub("_", name).strip()`),
// so strings.TrimSpace would leave a leading or trailing U+001C..U+001F in the
// filename. Verified against the reference: safe_filename("\x1ca.txt") is
// "a.txt".
func SafeFilename(name string) string {
	return textutil.PyStrip(unsafeChars.ReplaceAllString(name, "_"))
}

// EnsureNonemptyContent replaces a semantically empty result with a marker.
//
// Mirrors ensure_nonempty_tool_result (runtime.py:48): nil, a blank string, an
// empty block list, and a block list whose text is entirely blank all count as
// empty. Non-text blocks (images) are never considered empty on their own.
//
// "Blank" is Python's str.strip(), not Go's strings.TrimSpace: the reference
// writes `not content.strip()` (runtime.py:53) and `not text_payload.strip()`
// (runtime.py:58), which also remove U+001C..U+001F. Verified against the
// reference: ensure_nonempty_tool_result("t", "\x1c") returns the marker.
func EnsureNonemptyContent(toolName string, content core.Content) core.Content {
	marker := EmptyToolResultMessage(toolName)

	if content.IsZero() {
		return core.TextContent(marker)
	}
	if content.IsText() {
		if textutil.PyStrip(content.Text) == "" {
			return core.TextContent(marker)
		}
		return content
	}
	if len(content.Blocks) == 0 {
		return core.TextContent(marker)
	}
	// Only a block list that is ALL text and ALL blank counts as empty.
	allText, textPayload := true, strings.Builder{}
	for _, b := range content.Blocks {
		if b.Type != "text" {
			allText = false
			break
		}
		textPayload.WriteString(b.Text)
	}
	if allText && textutil.PyStrip(textPayload.String()) == "" {
		return core.TextContent(marker)
	}
	return content
}

// RenderToolResultReference mirrors _render_tool_result_reference
// (helpers.py:512). The wording is reproduced verbatim because the model reads
// it and must be told where the full output lives.
func RenderToolResultReference(referencePath string, originalSize int, preview string, truncatedPreview bool, maxChars int) string {
	var b strings.Builder
	b.WriteString("[tool output persisted]\n")
	b.WriteString("Full output saved to workspace path: ")
	b.WriteString(referencePath)
	b.WriteString("\nOriginal size: ")
	b.WriteString(itoa(originalSize))
	b.WriteString(" chars\nPreview:\n")
	b.WriteString(preview)
	if truncatedPreview {
		b.WriteString("\n...\nPreview is also truncated.")
	}
	b.WriteString("\nResult truncated. Read the saved file if you need the complete output.")
	result := b.String()
	if maxChars > 0 && len(result) > maxChars {
		return "[truncated: " + referencePath + "]"
	}
	return result
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// MaybePersistToolResult offloads oversized text into the workspace.
//
// Mirrors maybe_persist_tool_result (helpers.py:580). Returns content unchanged
// when it fits, when there is no workspace, or when maxChars is not positive.
//
// The write is atomic (temp file in the target directory, then rename) and
// fsynced, matching _write_text_atomic (helpers.py:553). The temp file is
// created beside its destination rather than in a system temp directory, so a
// crash cannot leave a partial file where a reader would find it.
func MaybePersistToolResult(workspace, sessionKey, toolCallID, content string, maxChars int) string {
	if workspace == "" || maxChars <= 0 || len(content) <= maxChars {
		return content
	}

	root := filepath.Join(workspace, toolResultsDir)
	bucketName := SafeFilename(sessionKey)
	if bucketName == "" {
		bucketName = "default"
	}
	bucket := filepath.Join(root, bucketName)

	if err := os.MkdirAll(bucket, 0o755); err != nil {
		// Persisting is an optimization; failing it must not fail the turn.
		return TruncateText(content, maxChars)
	}

	cleanupToolResultBuckets(root, bucket)

	path := filepath.Join(bucket, SafeFilename(toolCallID)+".txt")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := writeFileAtomic(path, content, 0o600); err != nil {
			return TruncateText(content, maxChars)
		}
	}

	preview := content
	truncated := false
	if len(content) > toolResultPreviewChars {
		preview = content[:toolResultPreviewChars]
		truncated = true
	}

	// The reference resolves symlinks so the model receives a usable absolute
	// path. EvalSymlinks needs the file to exist, which it does by now; fall
	// back to Abs if the filesystem refuses.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved, err = filepath.Abs(path)
		if err != nil {
			resolved = path
		}
	}
	return RenderToolResultReference(resolved, len(content), preview, truncated, maxChars)
}

// cleanupToolResultBuckets mirrors _cleanup_tool_result_buckets
// (helpers.py:540): delete buckets older than the retention window, then keep
// only the most recently modified ones. Failures are ignored, because a stale
// bucket is harmless and must not break a turn.
func cleanupToolResultBuckets(root, current string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var siblings []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		if p == current {
			continue
		}
		siblings = append(siblings, p)
	}

	cutoff := time.Now().Add(-time.Duration(toolResultRetentionSecs) * time.Second)
	var kept []string
	for _, p := range siblings {
		if bucketMtime(p).Before(cutoff) {
			os.RemoveAll(p)
			continue
		}
		kept = append(kept, p)
	}

	keep := toolResultMaxBuckets - 1
	if keep < 0 {
		keep = 0
	}
	if len(kept) <= keep {
		return
	}
	sort.Slice(kept, func(i, j int) bool {
		return bucketMtime(kept[i]).After(bucketMtime(kept[j]))
	})
	for _, p := range kept[keep:] {
		os.RemoveAll(p)
	}
}

func bucketMtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// writeFileAtomic writes content via a temp file in the same directory.
func writeFileAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(suffix[:])+".tmp")

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// NormalizeToolResult prepares one tool result for the model.
//
// Mirrors ContextGovernor.normalize_tool_result (context_governance.py:709):
// empty results are replaced with a marker, and oversized text is offloaded
// unless the tool is exempt. For block content only text blocks are offloaded;
// image blocks pass through untouched, because replacing them would change
// what the model sees rather than merely where it reads it.
func NormalizeToolResult(workspace, sessionKey, toolCallID, toolName string, content core.Content, maxChars int) core.Content {
	content = EnsureNonemptyContent(toolName, content)
	if toolResultOffloadExempt[toolName] {
		return content
	}

	persist := func(text, callID string) string {
		out := MaybePersistToolResult(workspace, sessionKey, callID, text, maxChars)
		// Without a workspace the reference cannot hand back a path, so it
		// truncates instead of pretending a file exists.
		if workspace == "" {
			return TruncateText(out, maxChars)
		}
		return out
	}

	if content.IsText() {
		return core.TextContent(persist(content.Text, toolCallID))
	}
	if len(content.Blocks) == 0 {
		return content
	}

	blocks := make([]core.ContentBlock, len(content.Blocks))
	copy(blocks, content.Blocks)
	changed := false
	for i, b := range blocks {
		if b.Type != "text" {
			continue
		}
		next := persist(b.Text, toolCallID+"_text_"+itoa(i))
		if next != b.Text {
			blocks[i].Text = next
			changed = true
		}
	}
	if !changed {
		return content
	}
	return core.BlockContent(blocks)
}
