package filediff

import (
	"regexp"
	"strconv"
	"strings"
)

// UnifiedDiffPayload is the compact unified diff the reference hands to the
// WebUI (utils/file_edit_events.py:217-222).
//
// The reference builds a plain dict; this port uses a typed struct with the
// same JSON keys so the value can be dropped into an event payload map without
// a second, untyped representation of the same four fields.
type UnifiedDiffPayload struct {
	Format    string `json:"format"`
	Context   int    `json:"context"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text"`
}

// UnifiedDiffOptions are the keyword arguments of build_unified_diff_payload.
//
// The reference has defaults, so a Go zero value cannot express "unset" for
// ContextLines (0 is meaningful). Use DefaultUnifiedDiffOptions and override the
// fields you need.
type UnifiedDiffOptions struct {
	// FromFile/ToFile are the ---/+++ labels (defaults "before"/"after").
	FromFile string
	ToFile   string
	// ContextLines is passed through to the payload verbatim, but the diff
	// itself is rendered with max(0, ContextLines) — the reference does the
	// same, so a negative value is visible in the payload and absent from the
	// text.
	ContextLines int
	// MaxLines and MaxLineChars bound the emitted body.
	MaxLines     int
	MaxLineChars int
	// Diff reuses an already-computed alignment instead of recomputing it.
	Diff *FileDiff
}

// DefaultUnifiedDiffOptions returns the reference's keyword defaults
// (utils/file_edit_events.py:194-198).
func DefaultUnifiedDiffOptions() UnifiedDiffOptions {
	return UnifiedDiffOptions{
		FromFile:     "before",
		ToFile:       "after",
		ContextLines: DiffContextLines,
		MaxLines:     MaxDiffLines,
		MaxLineChars: MaxDiffLineChars,
	}
}

// BuildUnifiedDiffPayload ports build_unified_diff_payload
// (utils/file_edit_events.py:190-222).
//
// It returns nil when either side is absent, when the diff has no lines, or
// when the line limit suppressed every body line — the three cases where the
// reference returns None.
//
// This function has no caller in the port yet: it feeds the WebUI file-edit
// event payload, and the event-payload builder (build_file_edit_end_event) is
// part of the file-edit tracker layer that has not been ported. It is included
// because it is the last piece of file_edit_events.py that depends on FileDiff,
// and because leaving it out would make the package's coverage of the reference
// file ambiguous.
func BuildUnifiedDiffPayload(before, after *string, opts UnifiedDiffOptions) *UnifiedDiffPayload {
	if before == nil || after == nil {
		return nil
	}
	diff := opts.Diff
	if diff == nil {
		d := FileDiffFromText(*before, *after)
		diff = &d
	}
	diffLines := diff.UnifiedLines(opts.FromFile, opts.ToFile, max(0, opts.ContextLines))
	if len(diffLines) == 0 {
		return nil
	}
	limited, truncated, emitted := limitUnifiedDiffLines(diffLines, opts.MaxLines, opts.MaxLineChars)
	if emitted == 0 {
		return nil
	}
	return &UnifiedDiffPayload{
		Format:    "unified",
		Context:   opts.ContextLines,
		Truncated: truncated,
		Text:      strings.Join(limited, "\n"),
	}
}

// limitUnifiedDiffLines ports _limit_unified_diff_lines
// (utils/file_edit_events.py:225-278). It returns the kept lines, whether
// anything was dropped, and how many body lines were emitted.
func limitUnifiedDiffLines(diffLines []string, maxLines, maxLineChars int) (limited []string, truncated bool, emitted int) {
	bodyLimit := max(0, maxLines)
	lineCharLimit := max(0, maxLineChars)
	index := 0
	for index < len(diffLines) {
		line := diffLines[index]
		if !strings.HasPrefix(line, "@@ ") {
			limited = append(limited, line)
			index++
			continue
		}

		hunkHeader := line
		var hunkBody []string
		index++
		for index < len(diffLines) && !strings.HasPrefix(diffLines[index], "@@ ") {
			hunkBody = append(hunkBody, diffLines[index])
			index++
		}

		remaining := bodyLimit - emitted
		if remaining <= 0 {
			truncated = true
			break
		}

		selected := hunkBody
		if remaining < len(hunkBody) {
			selected = hunkBody[:remaining]
		}
		if len(selected) < len(hunkBody) {
			truncated = true
		}

		selected, truncatedLine := limitUnifiedDiffLineChars(selected, lineCharLimit)
		truncated = truncated || truncatedLine
		if len(selected) == len(hunkBody) {
			limited = append(limited, hunkHeader)
		} else {
			limited = append(limited, rewriteHunkHeaderForBody(hunkHeader, selected))
		}
		limited = append(limited, selected...)
		emitted += len(selected)

		if truncated && emitted >= bodyLimit {
			break
		}
	}
	return limited, truncated, emitted
}

// limitUnifiedDiffLineChars ports _limit_unified_diff_line_chars
// (utils/file_edit_events.py:281-302).
//
// The limit is a CHARACTER count, not a byte count: the reference compares
// len(content) and slices content[:max_line_chars], both of which are character
// operations on a str.
func limitUnifiedDiffLineChars(lines []string, maxLineChars int) ([]string, bool) {
	if maxLineChars <= 0 {
		return lines, false
	}
	limited := make([]string, 0, len(lines))
	truncated := false
	for _, line := range lines {
		if line == "" {
			limited = append(limited, line)
			continue
		}
		marker := line[0]
		if marker != ' ' && marker != '+' && marker != '-' {
			limited = append(limited, line)
			continue
		}
		content := line[1:]
		runes := []rune(content)
		if len(runes) > maxLineChars {
			limited = append(limited, string(marker)+string(runes[:maxLineChars]))
			truncated = true
		} else {
			limited = append(limited, line)
		}
	}
	return limited, truncated
}

// hunkHeaderRe is _HUNK_HEADER_RE (utils/file_edit_events.py:305-308).
//
// Python's \d also matches non-ASCII decimal digits; Go's matches [0-9] only.
// The headers this is applied to are always produced by formatHunkRange, which
// emits ASCII digits, so the two agree on every reachable input.
var hunkHeaderRe = regexp.MustCompile(
	`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// rewriteHunkHeaderForBody ports _rewrite_hunk_header_for_body
// (utils/file_edit_events.py:311-333). It rewrites the header's line counts to
// match a truncated body, leaving the recorded start positions alone.
func rewriteHunkHeaderForBody(header string, body []string) string {
	m := hunkHeaderRe.FindStringSubmatch(header)
	if m == nil {
		return header
	}
	oldLines, newLines := 0, 0
	for _, line := range body {
		if line == "" {
			continue
		}
		switch line[0] {
		case ' ', '-':
			oldLines++
		}
		switch line[0] {
		case ' ', '+':
			newLines++
		}
	}
	oldStart, err := strconv.Atoi(m[1])
	if err != nil {
		return header
	}
	newStart, err := strconv.Atoi(m[3])
	if err != nil {
		return header
	}
	section := m[5]
	return "@@ -" + formatHunkRange(oldStart, oldLines) +
		" +" + formatHunkRange(newStart, newLines) + " @@" + section
}
