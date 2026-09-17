package telegram

import (
	"slices"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// SplitMarkdown splits raw Telegram Markdown without leaving fenced code blocks
// unbalanced. Port of _split_telegram_markdown (runtime.py:100-172).
//
// WHY THIS IS INDEXED BY CHARACTER. Every index below is a Python str index,
// and Python indexes by code point. The whole function therefore runs on a
// []rune: content[:max_len], rfind, count("```"), len(fence) and startswith all
// mean CHARACTER positions and CHARACTER lengths. A byte-indexed translation
// would agree on ASCII and silently diverge on the first emoji, CJK character
// or combining mark — and Telegram's own limits are character counts too.
//
// The contract, taken from the reference's control flow rather than from its
// docstring:
//
//   - "" and whitespace-only input (Python whitespace, not Go's) yield no
//     chunks at all;
//   - leading whitespace is stripped once, up front;
//   - a cut prefers the last newline within the budget, then the last space,
//     then a hard cut at the budget;
//   - a cut that lands INSIDE a fenced block is moved back to the opening
//     fence, and the closing fence plus the re-opened fence are re-emitted, so
//     every chunk is a self-contained Markdown document. The concatenation of
//     the chunks is therefore NOT the input: fence markers are duplicated and
//     whitespace at each cut point is dropped. That is the reference's
//     behaviour, verified by differential test, not an implementation slip.
//
// DIVERGENCE (deliberate, documented): the reference LOOPS FOREVER when
// max_len <= 0, because the hard cut at `pos = max_len` consumes nothing and
// `content.lstrip()` cannot shrink the string. Reproducing a hang is not a
// useful contract, so maxLen < 1 returns the whole input as a single chunk.
// Callers in this port only ever pass MaxMessageLen or a positive re-split
// budget, so the branch is unreachable in production.
func SplitMarkdown(content string, maxLen int) []string {
	if content == "" {
		return []string{}
	}
	content = textutil.PyLStrip(content)
	if content == "" {
		return []string{}
	}
	runes := []rune(content)
	if len(runes) <= maxLen {
		return []string{string(runes)}
	}
	if maxLen < 1 {
		return []string{string(runes)}
	}

	// fenceLine returns the text from fencePos to the end of that line.
	fenceLine := func(fencePos int) string {
		lineEnd := indexRuneFrom(runes, '\n', fencePos)
		if lineEnd < 0 {
			return string(runes[fencePos:])
		}
		return string(runes[fencePos:lineEnd])
	}

	// splitInsideFencedCodeBlock reports whether the first pos characters leave
	// an odd number of ``` markers open, and if so where the block opened.
	splitInsideFencedCodeBlock := func(pos int) (bool, int, string) {
		if countFence(runes[:pos])%2 == 0 {
			return false, -1, ""
		}
		opening := lastFenceIndex(runes[:pos])
		if opening < 0 {
			return true, -1, "```"
		}
		return true, opening, fenceLine(opening)
	}

	chunks := []string{}
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}

		cut := runes[:maxLen]
		pos := lastIndexRune(cut, '\n')
		if pos <= 0 {
			pos = lastIndexRune(cut, ' ')
		}
		if pos <= 0 {
			pos = maxLen
		}

		insideCode, opening, fence := splitInsideFencedCodeBlock(pos)
		if insideCode {
			if opening > 0 {
				pos = opening
			} else {
				closing := "\n```"
				closingLen := utf8.RuneCountInString(closing)
				fenceLen := utf8.RuneCountInString(fence)
				minCodePos := fenceLen
				if hasRunePrefix(runes, fence+"\n") {
					minCodePos++
				}
				switch {
				case pos < minCodePos:
					// When the only break in range is the opening fence
					// newline, cutting there re-emits the same fence and never
					// advances, so the budget is reduced instead.
					if minCodePos+closingLen >= maxLen {
						chunks = append(chunks, string(runes[:maxLen]))
						runes = []rune(textutil.PyLStrip(string(runes[maxLen:])))
						continue
					}
					pos = recutPos(runes, maxLen-closingLen, minCodePos)
				case pos+closingLen > maxLen:
					budget := maxLen - closingLen
					if budget <= minCodePos {
						chunks = append(chunks, string(runes[:maxLen]))
						runes = []rune(textutil.PyLStrip(string(runes[maxLen:])))
						continue
					}
					pos = recutPos(runes, budget, minCodePos)
				}
				if pos <= minCodePos {
					chunks = append(chunks, string(runes[:maxLen]))
					runes = []rune(textutil.PyLStrip(string(runes[maxLen:])))
					continue
				}
				chunks = append(chunks, string(runes[:pos])+closing)
				remainder := runes[pos:]
				if len(remainder) > 0 && remainder[0] == '\n' {
					remainder = remainder[1:]
				}
				runes = append(append([]rune(fence), '\n'), remainder...)
				continue
			}
		}

		chunks = append(chunks, string(runes[:pos]))
		runes = []rune(textutil.PyLStrip(string(runes[pos:])))
	}
	return chunks
}

// recutPos re-breaks a fenced block that would not fit: the last newline at or
// after minCodePos, else the last space at or after minCodePos, else the budget
// itself. Port of the `recut`/`adjusted` block that the reference repeats in
// both branches of the fence handling.
func recutPos(runes []rune, budget, minCodePos int) int {
	if budget > len(runes) {
		budget = len(runes)
	}
	if budget < 0 {
		budget = 0
	}
	recut := runes[:budget]
	adjusted := lastIndexRuneFrom(recut, '\n', minCodePos)
	if adjusted < minCodePos {
		adjusted = lastIndexRuneFrom(recut, ' ', minCodePos)
	}
	if adjusted > minCodePos {
		return adjusted
	}
	return budget
}

// countFence counts non-overlapping "```" occurrences, which is what Python's
// str.count does. "```“" counts once and "``````" counts twice.
func countFence(s []rune) int {
	n := 0
	for i := 0; i+3 <= len(s); {
		if s[i] == '`' && s[i+1] == '`' && s[i+2] == '`' {
			n++
			i += 3
			continue
		}
		i++
	}
	return n
}

// lastFenceIndex is Python's str.rfind("```").
func lastFenceIndex(s []rune) int {
	for i := len(s) - 3; i >= 0; i-- {
		if s[i] == '`' && s[i+1] == '`' && s[i+2] == '`' {
			return i
		}
	}
	return -1
}

// indexRuneFrom is Python's str.find(ch, start): the index is relative to the
// WHOLE string, not to the start offset, and an out-of-range start yields -1
// rather than panicking.
func indexRuneFrom(s []rune, target rune, from int) int {
	if from < 0 {
		from = 0
	}
	if from > len(s) {
		return -1
	}
	idx := slices.Index(s[from:], target)
	if idx < 0 {
		return -1
	}
	return idx + from
}

// lastIndexRune is Python's str.rfind(ch).
func lastIndexRune(s []rune, target rune) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == target {
			return i
		}
	}
	return -1
}

// lastIndexRuneFrom is Python's str.rfind(ch, start).
func lastIndexRuneFrom(s []rune, target rune, from int) int {
	if from < 0 {
		from = 0
	}
	for i := len(s) - 1; i >= from; i-- {
		if s[i] == target {
			return i
		}
	}
	return -1
}

// hasRunePrefix is Python's str.startswith for a rune slice.
func hasRunePrefix(s []rune, prefix string) bool {
	p := []rune(prefix)
	if len(p) > len(s) {
		return false
	}
	for i, r := range p {
		if s[i] != r {
			return false
		}
	}
	return true
}

// MarkdownHTMLChunk is one raw-Markdown / rendered-HTML pair.
type MarkdownHTMLChunk struct {
	// Markdown is the raw chunk that produced HTML.
	Markdown string
	// HTML is the rendered Telegram HTML.
	HTML string
}

// SplitMarkdownHTMLChunks returns raw Markdown and rendered HTML chunk pairs
// that both fit their limits. Port of _split_telegram_markdown_html_chunks
// (runtime.py:390-412).
//
// Markdown can GROW when rendered (HTML tags and entities), so a chunk whose
// HTML overflows is re-split from the raw Markdown with a proportionally
// smaller budget rather than by slicing HTML, which would corrupt tags.
//
// The reference raises ValueError in two places; Go returns an error with the
// same message text.
func SplitMarkdownHTMLChunks(content string, maxHTMLLen int) ([]MarkdownHTMLChunk, error) {
	chunks := []MarkdownHTMLChunk{}
	pending := SplitMarkdown(content, MaxMessageLen)
	for len(pending) > 0 {
		chunk := pending[0]
		pending = pending[1:]
		html := MarkdownToHTML(chunk)
		if utf8.RuneCountInString(html) <= maxHTMLLen {
			chunks = append(chunks, MarkdownHTMLChunk{Markdown: chunk, HTML: html})
			continue
		}

		chunkLen := utf8.RuneCountInString(chunk)
		htmlLen := utf8.RuneCountInString(html)
		// int(len(chunk) * max_html_len / len(html)) - 8, then clamped to
		// [1, len(chunk)-1]. Python's int() truncates toward zero; the operand
		// is positive here, so integer division is the same.
		nextLimit := int(float64(chunkLen)*float64(maxHTMLLen)/float64(htmlLen)) - 8
		if nextLimit < 1 {
			nextLimit = 1
		}
		if nextLimit > chunkLen-1 {
			nextLimit = chunkLen - 1
		}
		if nextLimit <= 0 {
			return nil, errHTMLTokenTooLarge
		}
		parts := SplitMarkdown(chunk, nextLimit)
		if len(parts) == 1 && parts[0] == chunk {
			return nil, errUnableToSplit
		}
		pending = append(append([]string{}, parts...), pending...)
	}
	return chunks, nil
}

// SplitMarkdownHTML returns only the rendered HTML chunks. Port of
// _split_telegram_markdown_html (runtime.py:414-417).
func SplitMarkdownHTML(content string, maxHTMLLen int) ([]string, error) {
	chunks, err := SplitMarkdownHTMLChunks(content, maxHTMLLen)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, chunk.HTML)
	}
	return out, nil
}
