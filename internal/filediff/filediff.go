package filediff

import (
	"strconv"
	"strings"
)

// Diff-size constants, verbatim from utils/file_edit_events.py:15-17.
const (
	// MaxDiffLines caps the number of unified-diff body lines emitted into a
	// WebUI payload (_MAX_DIFF_LINES).
	MaxDiffLines = 500
	// MaxDiffLineChars caps the number of characters of one diff body line
	// (_MAX_DIFF_LINE_CHARS).
	MaxDiffLineChars = 1200
	// DiffContextLines is the number of context lines on each side of a hunk
	// (_DIFF_CONTEXT_LINES).
	DiffContextLines = 3
)

// FileDiff is one line alignment shared by edit summaries and activity events.
//
// Ports FileDiff (utils/file_edit_events.py:49-132). The Python dataclass has
// slots and no defaults; the zero value here is a valid empty diff.
type FileDiff struct {
	// BeforeLines and AfterLines are the CRLF-normalised, split-lined inputs.
	BeforeLines []string
	AfterLines  []string
	// Opcodes is the Indel alignment; see IndelOpcodes.
	Opcodes []Opcode
	// Added and Deleted are the line counts derived from Opcodes.
	Added   int
	Deleted int
}

// FileDiffFromText ports FileDiff.from_text (utils/file_edit_events.py:58-69).
//
// Both sides are CRLF-normalised and then split with Python's splitlines
// semantics, so a lone "\r", "\v", "\f", U+001C..U+001E, U+0085, U+2028 or
// U+2029 is a line break here exactly as it is in the reference.
func FileDiffFromText(before, after string) FileDiff {
	beforeLines := pySplitLines(strings.ReplaceAll(before, "\r\n", "\n"))
	afterLines := pySplitLines(strings.ReplaceAll(after, "\r\n", "\n"))
	opcodes := IndelOpcodes(beforeLines, afterLines)

	added, deleted := 0, 0
	for _, code := range opcodes {
		// The "replace" arms are dead for Indel, which never emits that tag;
		// they are reproduced because the reference tests for it here.
		if code.Tag == TagReplace || code.Tag == TagDelete {
			deleted += code.SrcEnd - code.SrcStart
		}
		if code.Tag == TagReplace || code.Tag == TagInsert {
			added += code.DestEnd - code.DestStart
		}
	}
	return FileDiff{
		BeforeLines: beforeLines,
		AfterLines:  afterLines,
		Opcodes:     opcodes,
		Added:       added,
		Deleted:     deleted,
	}
}

// Matches ports FileDiff.matches (utils/file_edit_events.py:71-76): it checks
// that an observed file still has the compared line contents, so a cached diff
// can be reused instead of recomputed.
func (d FileDiff) Matches(before, after string) bool {
	return equalStrings(d.BeforeLines, pySplitLines(strings.ReplaceAll(before, "\r\n", "\n"))) &&
		equalStrings(d.AfterLines, pySplitLines(strings.ReplaceAll(after, "\r\n", "\n")))
}

// Groups ports FileDiff._groups (utils/file_edit_events.py:78-106).
//
// The first and last opcodes, when they are "equal", are shortened to `context`
// lines so the diff does not open or close with a long unchanged run; interior
// "equal" runs longer than 2*context are split into a trailing `context`-line
// piece, a group boundary, and a leading `context`-line piece. The final group
// is emitted only when it contains at least one non-equal opcode.
func (d FileDiff) Groups(context int) [][]Opcode {
	if len(d.Opcodes) == 0 {
		return nil
	}
	codes := make([]Opcode, len(d.Opcodes))
	copy(codes, d.Opcodes)

	// NOTE: the reference rebinds `first` to codes[0] and then, for the last
	// element, re-reads codes[-1] after the first rewrite. A single-element
	// all-equal diff therefore has both rewrites applied to the same opcode,
	// which is why codes[0] is re-read here rather than reusing `first`.
	if codes[0].Tag == TagEqual {
		first := codes[0]
		codes[0] = Opcode{
			Tag:       TagEqual,
			SrcStart:  max(first.SrcStart, first.SrcEnd-context),
			SrcEnd:    first.SrcEnd,
			DestStart: max(first.DestStart, first.DestEnd-context),
			DestEnd:   first.DestEnd,
		}
	}
	if codes[len(codes)-1].Tag == TagEqual {
		last := codes[len(codes)-1]
		codes[len(codes)-1] = Opcode{
			Tag:       TagEqual,
			SrcStart:  last.SrcStart,
			SrcEnd:    min(last.SrcEnd, last.SrcStart+context),
			DestStart: last.DestStart,
			DestEnd:   min(last.DestEnd, last.DestStart+context),
		}
	}

	var groups [][]Opcode
	var group []Opcode
	for _, code := range codes {
		i1, i2 := code.SrcStart, code.SrcEnd
		j1, j2 := code.DestStart, code.DestEnd
		if code.Tag == TagEqual && i2-i1 > context*2 {
			group = append(group, Opcode{
				Tag:       code.Tag,
				SrcStart:  i1,
				SrcEnd:    i1 + context,
				DestStart: j1,
				DestEnd:   j1 + context,
			})
			groups = append(groups, group)
			group = nil
			i1, j1 = i2-context, j2-context
		}
		group = append(group, Opcode{
			Tag:       code.Tag,
			SrcStart:  i1,
			SrcEnd:    i2,
			DestStart: j1,
			DestEnd:   j2,
		})
	}
	if hasNonEqual(group) {
		groups = append(groups, group)
	}
	return groups
}

// UnifiedLines ports FileDiff.unified_lines (utils/file_edit_events.py:108-132).
//
// It returns nil when nothing was added or deleted, matching the reference's
// generator, which yields nothing in that case.
func (d FileDiff) UnifiedLines(fromfile, tofile string, context int) []string {
	if d.Added == 0 && d.Deleted == 0 {
		return nil
	}
	out := []string{"--- " + fromfile, "+++ " + tofile}
	for _, group := range d.Groups(context) {
		first, last := group[0], group[len(group)-1]
		oldCount := last.SrcEnd - first.SrcStart
		newCount := last.DestEnd - first.DestStart
		oldRange := formatHunkRange(first.SrcStart+boolToInt(oldCount != 0), oldCount)
		newRange := formatHunkRange(first.DestStart+boolToInt(newCount != 0), newCount)
		out = append(out, "@@ -"+oldRange+" +"+newRange+" @@")
		for _, code := range group {
			if code.Tag == TagEqual {
				for _, line := range d.BeforeLines[code.SrcStart:code.SrcEnd] {
					out = append(out, " "+line)
				}
			}
			if code.Tag == TagReplace || code.Tag == TagDelete {
				for _, line := range d.BeforeLines[code.SrcStart:code.SrcEnd] {
					out = append(out, "-"+line)
				}
			}
			if code.Tag == TagReplace || code.Tag == TagInsert {
				for _, line := range d.AfterLines[code.DestStart:code.DestEnd] {
					out = append(out, "+"+line)
				}
			}
		}
	}
	return out
}

// LineDiffStats ports line_diff_stats (utils/file_edit_events.py:182-187): the
// (added, deleted) pair for a UTF-8 text line-level diff, or (0, 0) when either
// side is absent.
func LineDiffStats(before, after *string) (added, deleted int) {
	if before == nil || after == nil {
		return 0, 0
	}
	d := FileDiffFromText(*before, *after)
	return d.Added, d.Deleted
}

// formatHunkRange ports _format_hunk_range (utils/file_edit_events.py:336-337).
func formatHunkRange(start, lineCount int) string {
	if lineCount == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(lineCount)
}

func hasNonEqual(group []Opcode) bool {
	for _, code := range group {
		if code.Tag != TagEqual {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
