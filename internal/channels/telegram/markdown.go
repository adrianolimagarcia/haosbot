package telegram

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// Errors raised by SplitMarkdownHTMLChunks, with the reference's message text.
var (
	// errHTMLTokenTooLarge is the ValueError at runtime.py:406.
	errHTMLTokenTooLarge = errors.New("A rendered Telegram HTML token exceeds the message limit")
	// errUnableToSplit is the ValueError at runtime.py:409.
	errUnableToSplit = errors.New("Unable to split Telegram Markdown within the HTML limit")
)

// ---------------------------------------------------------------------------
// Plain-text escaping and preview stripping
// ---------------------------------------------------------------------------

// EscapeHTML escapes text for Telegram's HTML parse mode. Port of
// _escape_telegram_html (runtime.py:175-177).
//
// The three replacements are SEQUENTIAL, in this order, exactly as in the
// reference: the ampersands introduced by the later passes are not re-escaped.
// Quotes are deliberately left alone — Telegram's HTML subset does not need
// them escaped in text nodes.
func EscapeHTML(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	return strings.ReplaceAll(text, ">", "&gt;")
}

// ToolHintBlockquote renders a tool hint as an expandable blockquote. Port of
// _tool_hint_to_telegram_blockquote (runtime.py:180-182). An empty hint renders
// as the empty string, not as an empty blockquote.
func ToolHintBlockquote(text string) string {
	if text == "" {
		return ""
	}
	return "<blockquote expandable>" + EscapeHTML(text) + "</blockquote>"
}

var (
	mdBoldStarRe       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdBoldUnderscoreRe = regexp.MustCompile(`__(.+?)__`)
	mdStrikeRe         = regexp.MustCompile(`~~(.+?)~~`)
	mdInlineCodeRe     = regexp.MustCompile("`([^`]+)`")
)

// StripMarkdownInline strips markdown inline formatting. Port of _strip_md
// (runtime.py:185-191). The result is stripped with PYTHON's strip, not Go's
// TrimSpace — the two disagree on U+001C..U+001F, NBSP and six other code
// points (see internal/textutil).
func StripMarkdownInline(s string) string {
	s = mdBoldStarRe.ReplaceAllString(s, "$1")
	s = mdBoldUnderscoreRe.ReplaceAllString(s, "$1")
	s = mdStrikeRe.ReplaceAllString(s, "$1")
	s = mdInlineCodeRe.ReplaceAllString(s, "$1")
	return textutil.PyStrip(s)
}

var (
	mdCodeBlockRe  = regexp.MustCompile("```(?:[^\\n]*\\n)?([\\s\\S]*?)```")
	mdHeaderRe     = regexp.MustCompile(`(?m)^#{1,6}` + pySpaceClass + `+(.+)$`)
	mdBlockquoteRe = regexp.MustCompile(`(?m)^>` + pySpaceClass + `*(.*)$`)
	mdLinkRe       = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	mdBulletRe     = regexp.MustCompile(`(?m)^[-*]` + pySpaceClass + `+`)
	mdNumberedRe   = regexp.MustCompile(`(?m)^(\d+)\.` + pySpaceClass + `+`)
)

// StripMarkdownBlock strips block-level and inline markdown for a readable
// plain-text preview. Port of _strip_md_block (runtime.py:194-218).
//
// Used during streaming mid-edits so users see clean text instead of raw
// markdown while the response is still being generated.
//
// DIVERGENCE IN MECHANISM, NONE IN BEHAVIOUR: the reference's fifth and sixth
// substitutions use `(?<![a-zA-Z0-9])_([^_]+)_(?![a-zA-Z0-9])`. RE2 has no
// lookaround, so those two are a hand-written left-to-right scan
// (replaceItalicUnderscores) that reproduces the regex engine's scanning and
// backtracking exactly. The differential corpus covers it directly.
func StripMarkdownBlock(text string) string {
	text = mdCodeBlockRe.ReplaceAllString(text, "$1")
	text = mdHeaderRe.ReplaceAllString(text, "$1")
	text = mdBlockquoteRe.ReplaceAllString(text, "$1")
	text = mdBoldStarRe.ReplaceAllString(text, "$1")
	text = mdBoldUnderscoreRe.ReplaceAllString(text, "$1")
	text = replaceItalicUnderscores(text, func(inner string) string { return inner })
	text = mdStrikeRe.ReplaceAllString(text, "$1")
	text = mdInlineCodeRe.ReplaceAllString(text, "$1")
	text = mdLinkRe.ReplaceAllString(text, "$1")
	text = mdBulletRe.ReplaceAllString(text, "• ")
	return mdNumberedRe.ReplaceAllString(text, "$1. ")
}

// isASCIIAlnum is the [a-zA-Z0-9] class the reference's italic lookaround uses.
// It is ASCII on purpose: the reference writes an explicit range, not `\w`.
func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// replaceItalicUnderscores is the RE2-safe translation of
//
//	re.sub(r'(?<![a-zA-Z0-9])_([^_]+)_(?![a-zA-Z0-9])', repl, s)
//
// The engine scans left to right and, at each position, first asserts the
// negative lookbehind, then requires `_`, then a non-empty run of
// non-underscores, then `_`, then the negative lookahead. `[^_]+` is greedy but
// can only end immediately before the NEXT underscore, so a match at position i
// is fully determined by i: it spans i..j where j is the next `_` after i, and
// it exists iff j >= i+2 and the character after j is not alphanumeric. When no
// match exists at i the engine advances by one; after a match it resumes at
// j+1. Both rules are reproduced below.
func replaceItalicUnderscores(s string, wrap func(inner string) string) string {
	runes := []rune(s)
	var b strings.Builder
	i := 0
	for i < len(runes) {
		if runes[i] == '_' && (i == 0 || !isASCIIAlnum(runes[i-1])) {
			j := -1
			for k := i + 1; k < len(runes); k++ {
				if runes[k] == '_' {
					j = k
					break
				}
			}
			if j >= i+2 && (j+1 == len(runes) || !isASCIIAlnum(runes[j+1])) {
				b.WriteString(wrap(string(runes[i+1 : j])))
				i = j + 1
				continue
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Pipe tables
// ---------------------------------------------------------------------------

var tableSeparatorRe = regexp.MustCompile(`^:?-+:?$`)

// displayWidth is the reference's `dw`: two columns for East Asian Wide/Fullwidth
// characters, one otherwise. It is a CHARACTER count, not a byte count.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if eastAsianWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isTableLine is the RE2 translation of `re.match(r'^\s*\|.+\|', line)`.
//
// `\s` is Python's Unicode whitespace, which is exactly textutil.PyLStrip's
// character set, so the leading run is stripped with PyLStrip rather than with
// Go's `\s` (ASCII) or strings.TrimSpace (missing U+001C..U+001F and friends).
// `.` never matches a newline in either engine and a "line" here is one line of
// a split, so `.+` reduces to "at least one more character": the match exists
// iff, after the leading whitespace, the text starts with `|` and contains
// another `|` at index >= 2.
func isTableLine(line string) bool {
	runes := []rune(textutil.PyLStrip(line))
	if len(runes) < 3 || runes[0] != '|' {
		return false
	}
	for i := 2; i < len(runes); i++ {
		if runes[i] == '|' {
			return true
		}
	}
	return false
}

// RenderTableBox converts a markdown pipe table to compact aligned text for
// <pre> display. Port of _render_table_box (runtime.py:221-252).
//
// Returns the original lines joined by "\n" when there is no separator row or
// no data row, which is how the caller detects "not a table".
//
// A row is treated as a separator when EVERY non-empty cell matches `^:?-+:?$`.
// When a row has no non-empty cells at all, `all(...)` over an empty generator
// is True, so such a row IS a separator — the reference does this, and the
// corpus pins it down.
func RenderTableBox(tableLines []string) string {
	rows := [][]string{}
	hasSep := false
	for _, line := range tableLines {
		trimmed := strings.Trim(textutil.PyStrip(line), "|")
		parts := strings.Split(trimmed, "|")
		cells := make([]string, 0, len(parts))
		for _, part := range parts {
			cells = append(cells, StripMarkdownInline(part))
		}
		allSeparator := true
		for _, cell := range cells {
			if cell == "" {
				continue
			}
			if !tableSeparatorRe.MatchString(cell) {
				allSeparator = false
				break
			}
		}
		if allSeparator {
			hasSep = true
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 || !hasSep {
		return strings.Join(tableLines, "\n")
	}

	ncols := 0
	for _, row := range rows {
		if len(row) > ncols {
			ncols = len(row)
		}
	}
	for i := range rows {
		for len(rows[i]) < ncols {
			rows[i] = append(rows[i], "")
		}
	}
	widths := make([]int, ncols)
	for c := 0; c < ncols; c++ {
		w := 0
		for _, row := range rows {
			if d := displayWidth(row[c]); d > w {
				w = d
			}
		}
		widths[c] = w
	}
	drawRow := func(cells []string) string {
		out := make([]string, len(cells))
		for i, cell := range cells {
			pad := widths[i] - displayWidth(cell)
			if pad < 0 {
				pad = 0
			}
			out[i] = cell + strings.Repeat(" ", pad)
		}
		return strings.Join(out, "  ")
	}

	out := []string{drawRow(rows[0])}
	rule := make([]string, ncols)
	for i, w := range widths {
		rule[i] = strings.Repeat("─", w)
	}
	out = append(out, strings.Join(rule, "  "))
	for _, row := range rows[1:] {
		out = append(out, drawRow(row))
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// Markdown -> Telegram HTML
// ---------------------------------------------------------------------------

var (
	// The header placeholder markers are U+27EA/U+27EB, which cannot appear in
	// the reference's output by accident: they are inserted after the table pass
	// and restored after every HTML escape.
	htmlHeaderOpen  = "⟪B⟫"
	htmlHeaderClose = "⟪/B⟫"

	htmlHeaderRe     = regexp.MustCompile(`(?m)^#{1,6}` + pySpaceClass + `+(.+)$`)
	htmlBlockquoteRe = regexp.MustCompile(`(?m)^>` + pySpaceClass + `*(.*)$`)
	htmlLinkRe       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	htmlBulletRe     = regexp.MustCompile(`(?m)^[-*]` + pySpaceClass + `+`)
	htmlNumberedRe   = regexp.MustCompile(`(?m)^(\d+)\.` + pySpaceClass + `+`)
)

// MarkdownToHTML converts markdown to Telegram-safe HTML. Port of
// _markdown_to_telegram_html (runtime.py:255-337).
//
// The pass order is load-bearing and is preserved exactly:
//
//  1. fenced code blocks are replaced by \x00CB<n>\x00 placeholders so no later
//     pass can touch their contents;
//     1.5 pipe-table runs are converted to box-drawing text and stored in the SAME
//     placeholder list, so a rendered table becomes a <pre><code> block;
//  2. inline code becomes \x00IC<n>\x00;
//  3. headers become placeholder bold markers — before escaping, because the
//     replacement text contains no escapable characters;
//  4. blockquote markers are dropped;
//  5. the whole text is HTML-escaped;
//  6. links, 7. bold, 8. italic, 9. strikethrough, 10. bullets and numbered
//     lists are rewritten — all AFTER escaping, so the tags they insert are
//     not escaped;
//     11./12. inline code and code blocks are restored, escaped inside their tags;
//  13. the header markers become real <b> tags.
//
// Steps 11 and 12 use Python's str.replace, which replaces every occurrence; the
// placeholders are unique per index, so this is equivalent to a plain
// replacement.
func MarkdownToHTML(text string) string {
	if text == "" {
		return ""
	}

	var codeBlocks []string
	saveCodeBlock := func(groups []string) string {
		codeBlocks = append(codeBlocks, groups[1])
		return "\x00CB" + strconv.Itoa(len(codeBlocks)-1) + "\x00"
	}
	text = replaceSubmatches(mdCodeBlockRe, text, saveCodeBlock)

	// 1.5. Tables. A run of consecutive table lines is rendered as a box and
	// stored as another code block; when the renderer declines (no separator
	// row) the original lines are kept verbatim.
	lines := strings.Split(text, "\n")
	rebuilt := make([]string, 0, len(lines))
	li := 0
	for li < len(lines) {
		if isTableLine(lines[li]) {
			tbl := []string{}
			for li < len(lines) && isTableLine(lines[li]) {
				tbl = append(tbl, lines[li])
				li++
			}
			box := RenderTableBox(tbl)
			if box != strings.Join(tbl, "\n") {
				codeBlocks = append(codeBlocks, box)
				rebuilt = append(rebuilt, "\x00CB"+strconv.Itoa(len(codeBlocks)-1)+"\x00")
			} else {
				rebuilt = append(rebuilt, tbl...)
			}
			continue
		}
		rebuilt = append(rebuilt, lines[li])
		li++
	}
	text = strings.Join(rebuilt, "\n")

	var inlineCodes []string
	saveInlineCode := func(groups []string) string {
		inlineCodes = append(inlineCodes, groups[1])
		return "\x00IC" + strconv.Itoa(len(inlineCodes)-1) + "\x00"
	}
	text = replaceSubmatches(mdInlineCodeRe, text, saveInlineCode)

	text = htmlHeaderRe.ReplaceAllString(text, htmlHeaderOpen+"$1"+htmlHeaderClose)
	text = htmlBlockquoteRe.ReplaceAllString(text, "$1")
	text = EscapeHTML(text)
	text = htmlLinkRe.ReplaceAllString(text, `<a href="$2">$1</a>`)
	text = mdBoldStarRe.ReplaceAllString(text, "<b>$1</b>")
	text = mdBoldUnderscoreRe.ReplaceAllString(text, "<b>$1</b>")
	text = replaceItalicUnderscores(text, func(inner string) string { return "<i>" + inner + "</i>" })
	text = mdStrikeRe.ReplaceAllString(text, "<s>$1</s>")
	text = htmlBulletRe.ReplaceAllString(text, "• ")
	text = htmlNumberedRe.ReplaceAllString(text, "$1. ")

	for i, code := range inlineCodes {
		text = strings.ReplaceAll(text, "\x00IC"+strconv.Itoa(i)+"\x00", "<code>"+EscapeHTML(code)+"</code>")
	}
	for i, code := range codeBlocks {
		text = strings.ReplaceAll(text, "\x00CB"+strconv.Itoa(i)+"\x00", "<pre><code>"+EscapeHTML(code)+"</code></pre>")
	}
	text = strings.ReplaceAll(text, htmlHeaderOpen, "<b>")
	return strings.ReplaceAll(text, htmlHeaderClose, "</b>")
}

// replaceSubmatches replaces every non-overlapping match of re in s with
// fn(groups), where groups[0] is the whole match. It is the Go equivalent of
// Python's `re.sub(pattern, function, s)`: matches are found left to right, the
// replacement text is not rescanned, and an unmatched optional group is the
// empty string.
func replaceSubmatches(re *regexp.Regexp, s string, fn func(groups []string) string) string {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		groups := make([]string, len(m)/2)
		for i := range groups {
			if m[2*i] >= 0 {
				groups[i] = s[m[2*i]:m[2*i+1]]
			}
		}
		b.WriteString(fn(groups))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
