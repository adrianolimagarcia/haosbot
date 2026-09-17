package api

import (
	"strconv"
	"strings"
)

// renderSafeMarkdown converts a small, deliberately limited subset of Markdown
// into HTML that is safe to hand to the WebUI's innerHTML.
//
// The security property is structural rather than filter-based: no byte of the
// input reaches the output without being HTML-escaped first, and the only tags
// that can appear in the result are the literals written by this file. Raw HTML
// in the source (model output, tool results, fetched web content) is therefore
// rendered as visible text and can never become markup.
//
// Supported: ATX headings, paragraphs, unordered/ordered lists, blockquotes,
// horizontal rules, fenced and inline code, bold, italic, and links. Images are
// rendered as links: a remote image would let attacker-influenced output make
// the operator's browser call an arbitrary host, and the WebUI's CSP blocks
// remote images anyway.
//
// Not supported (rendered as literal text): raw HTML, tables, nested lists,
// setext headings, autolinks, and reference-style links.
func renderSafeMarkdown(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")

	var out strings.Builder
	out.Grow(len(src) + 64)

	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])

		switch {
		case line == "":
			i++

		case fenceLen(line) > 0:
			marker := line[0]
			n := fenceLen(line)
			i++
			var code []string
			for i < len(lines) && !isClosingFence(strings.TrimSpace(lines[i]), marker, n) {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // consume the closing fence
			}
			out.WriteString("<pre><code>")
			out.WriteString(escapeHTML(strings.Join(code, "\n")))
			out.WriteString("</code></pre>\n")

		case isHorizontalRule(line):
			out.WriteString("<hr>\n")
			i++

		case headingLevel(line) > 0:
			level := headingLevel(line)
			tag := "h" + strconv.Itoa(level)
			out.WriteString("<" + tag + ">")
			out.WriteString(renderInline(strings.TrimSpace(line[level+1:])))
			out.WriteString("</" + tag + ">\n")
			i++

		case isBullet(line):
			out.WriteString("<ul>\n")
			for i < len(lines) {
				item, ok := bulletText(strings.TrimSpace(lines[i]))
				if !ok {
					break
				}
				out.WriteString("<li>")
				out.WriteString(renderInline(item))
				out.WriteString("</li>\n")
				i++
			}
			out.WriteString("</ul>\n")

		case isOrderedItem(line):
			out.WriteString("<ol>\n")
			for i < len(lines) {
				item, ok := orderedText(strings.TrimSpace(lines[i]))
				if !ok {
					break
				}
				out.WriteString("<li>")
				out.WriteString(renderInline(item))
				out.WriteString("</li>\n")
				i++
			}
			out.WriteString("</ol>\n")

		case strings.HasPrefix(line, ">"):
			var quoted []string
			for i < len(lines) {
				current := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(current, ">") {
					break
				}
				quoted = append(quoted, renderInline(strings.TrimSpace(strings.TrimPrefix(current, ">"))))
				i++
			}
			out.WriteString("<blockquote>")
			out.WriteString(strings.Join(quoted, "<br>"))
			out.WriteString("</blockquote>\n")

		default:
			out.WriteString("<p>")
			first := true
			for i < len(lines) {
				current := strings.TrimSpace(lines[i])
				if current == "" || isBlockStart(current) {
					break
				}
				if !first {
					out.WriteString("<br>")
				}
				out.WriteString(renderInline(current))
				first = false
				i++
			}
			out.WriteString("</p>\n")
		}
	}

	return out.String()
}

// inlineScanLimit bounds how far the inline scanner will look for a closing
// delimiter. Without it a pathological input such as a megabyte of "[" would
// make every position scan the whole remaining line (quadratic work).
const inlineScanLimit = 4096

// maxInlineDepth bounds recursive emphasis parsing so that adversarial nesting
// cannot exhaust the stack.
const maxInlineDepth = 8

const inlineSpecial = "`*_[!<>&\"'"

func isBlockStart(line string) bool {
	return fenceLen(line) > 0 ||
		isHorizontalRule(line) ||
		headingLevel(line) > 0 ||
		isBullet(line) ||
		isOrderedItem(line) ||
		strings.HasPrefix(line, ">")
}

func isHorizontalRule(line string) bool {
	if len(line) < 3 {
		return false
	}
	c := line[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(line); i++ {
		if line[i] != c {
			return false
		}
	}
	return true
}

func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' && n < 6 {
		n++
	}
	if n == 0 || n >= len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

// fenceLen reports the length of a code fence opener, or 0 when the line is not
// a fence.
func fenceLen(line string) int {
	if line == "" {
		return 0
	}
	c := line[0]
	if c != '`' && c != '~' {
		return 0
	}
	n := 0
	for n < len(line) && line[n] == c {
		n++
	}
	if n < 3 {
		return 0
	}
	return n
}

func isClosingFence(line string, marker byte, n int) bool {
	if len(line) == 0 || line[0] != marker {
		return false
	}
	count := 0
	for count < len(line) && line[count] == marker {
		count++
	}
	return count >= n && strings.TrimSpace(line[count:]) == ""
}

func isBullet(line string) bool {
	_, ok := bulletText(line)
	return ok
}

func bulletText(line string) (string, bool) {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') &&
		(line[1] == ' ' || line[1] == '\t') {
		return strings.TrimSpace(line[2:]), true
	}
	return "", false
}

func isOrderedItem(line string) bool {
	_, ok := orderedText(line)
	return ok
}

func orderedText(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i > 9 || i+1 >= len(line) {
		return "", false
	}
	if line[i] != '.' && line[i] != ')' {
		return "", false
	}
	if line[i+1] != ' ' && line[i+1] != '\t' {
		return "", false
	}
	return strings.TrimSpace(line[i+2:]), true
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

func escapeHTML(s string) string {
	if !strings.ContainsAny(s, "&<>\"'") {
		return s
	}
	return htmlEscaper.Replace(s)
}

func renderInline(text string) string {
	return renderInlineDepth(text, 0)
}

func renderInlineDepth(text string, depth int) string {
	if depth >= maxInlineDepth {
		return escapeHTML(text)
	}

	var out strings.Builder
	out.Grow(len(text) + 16)

	for i := 0; i < len(text); {
		j := strings.IndexAny(text[i:], inlineSpecial)
		if j < 0 {
			out.WriteString(escapeHTML(text[i:]))
			break
		}
		if j > 0 {
			out.WriteString(escapeHTML(text[i : i+j]))
			i += j
			continue
		}

		switch c := text[i]; {
		case c == '`':
			if end := indexWithin(text[i+1:], "`", inlineScanLimit); end >= 0 {
				out.WriteString("<code>")
				out.WriteString(escapeHTML(text[i+1 : i+1+end]))
				out.WriteString("</code>")
				i += end + 2
				continue
			}
			out.WriteString("&#96;")
			i++

		case (c == '*' || c == '_') && strings.HasPrefix(text[i:], string([]byte{c, c})):
			end := indexWithin(text[i+2:], string([]byte{c, c}), inlineScanLimit)
			if end > 0 {
				out.WriteString("<strong>")
				out.WriteString(renderInlineDepth(text[i+2:i+2+end], depth+1))
				out.WriteString("</strong>")
				i += end + 4
				continue
			}
			out.WriteString(escapeHTML(text[i : i+1]))
			i++

		case c == '*' || c == '_':
			if end := indexWithin(text[i+1:], string([]byte{c}), inlineScanLimit); end > 0 {
				out.WriteString("<em>")
				out.WriteString(renderInlineDepth(text[i+1:i+1+end], depth+1))
				out.WriteString("</em>")
				i += end + 2
				continue
			}
			out.WriteString(escapeHTML(text[i : i+1]))
			i++

		case c == '[' || (c == '!' && i+1 < len(text) && text[i+1] == '['):
			html, next, ok := renderInlineLink(text, i)
			if !ok {
				out.WriteString(escapeHTML(text[i : i+1]))
				i++
				continue
			}
			out.WriteString(html)
			i = next

		default:
			out.WriteString(escapeHTML(text[i : i+1]))
			i++
		}
	}

	return out.String()
}

// renderInlineLink renders "[label](url)" (or "![alt](url)", which becomes a
// link) starting at i. It reports ok=false when the construct is not a link, in
// which case the caller emits the current byte as text.
func renderInlineLink(text string, i int) (string, int, bool) {
	open := i
	if text[i] == '!' {
		open = i + 1
	}
	closeLabel := indexWithin(text[open+1:], "]", inlineScanLimit)
	if closeLabel < 0 {
		return "", i, false
	}
	labelEnd := open + 1 + closeLabel
	if labelEnd+1 >= len(text) || text[labelEnd+1] != '(' {
		return "", i, false
	}
	closeURL := indexWithin(text[labelEnd+2:], ")", inlineScanLimit)
	if closeURL < 0 {
		return "", i, false
	}
	end := labelEnd + 2 + closeURL + 1

	label := text[open+1 : labelEnd]
	raw := text[labelEnd+2 : labelEnd+2+closeURL]
	href, safe := safeLinkURL(raw)
	if !safe {
		// Keep the construct visible, but as inert text.
		return escapeHTML(text[i:end]), end, true
	}
	return `<a href="` + escapeHTML(href) + `" rel="noopener noreferrer nofollow">` + escapeHTML(label) + `</a>`, end, true
}

func indexWithin(text, delim string, limit int) int {
	if limit < len(text) {
		text = text[:limit]
	}
	return strings.Index(text, delim)
}

// safeLinkURL accepts only URLs whose scheme cannot execute script. Whitespace
// and control characters are stripped before the scheme test because browsers
// ignore them inside a URL, so "java\tscript:" would otherwise slip through.
func safeLinkURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	probe := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	if probe == "" {
		return "", false
	}
	if i := strings.IndexAny(probe, ":/?#"); i >= 0 && probe[i] == ':' {
		switch strings.ToLower(probe[:i]) {
		case "http", "https", "mailto":
		default:
			return "", false
		}
	}
	return raw, true
}
