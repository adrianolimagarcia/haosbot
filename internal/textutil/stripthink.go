// Package textutil holds text helpers shared across the port.
//
// They live here rather than in one consumer because more than one subsystem
// needs them, and a duplicated copy of a subtle rule is how this project
// already drifted once (a shared marker list was reimplemented per package).
package textutil

import (
	"regexp"
	"strings"
)

// The tag names the reference recognises. Order is significant: it is the
// alternation order of the Python regex, and the backtracking there decides
// which prefix is tried first.
var thinkingTags = []string{"think", "thinking", "thought"}

// thinkingTagAlt is _THINKING_TAG: (?:think|thinking|thought).
const thinkingTagAlt = "(?:think|thinking|thought)"

// partialThinkingTag is _PARTIAL_THINKING_TAG, whose prefix list was computed
// from every prefix of every tag, longest first:
//
//	</?(?:thinking|thinkin|thought|thinki|though|thoug|think|thou|thin|tho|thi|th|t)>?
const partialThinkingTag = `</?(?:thinking|thinkin|thought|thinki|though|thoug|think|thou|thin|tho|thi|th|t)>?`

// Go's regexp is RE2: it has neither backreferences nor lookahead, and the
// reference uses both. Each is replaced by an equivalent that does not need
// them, rather than by an approximation.

// wellFormedTag matches a closed block for one specific tag.
//
// Python writes this as `<(?P<tag>think|thinking|thought)>[\s\S]*?</(?P=tag)>`,
// relying on a backreference to require the same tag on both ends. Three
// separate patterns are equivalent here because the tag set is fixed and the
// closing literal is unambiguous: `<think>` cannot match the opening of
// `<thinking>` since the character after "think" would have to be `>`.
var wellFormedTag = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(thinkingTags))
	for _, tag := range thinkingTags {
		out = append(out, regexp.MustCompile(`(?s)<`+tag+`>.*?</`+tag+`>`))
	}
	return out
}()

// These patterns are written with pySpaceClass and pyEnd rather than `\s` and
// `$`, because Go's regexp dialect disagrees with Python's on both:
//
//   - Go's `\s` is the ASCII set `[\t\n\f\r ]`; Python's `\s` is str.isspace(),
//     which additionally matches U+000B, U+001C..U+001F, U+0085, U+00A0,
//     U+1680, U+2000..U+200A, U+2028, U+2029, U+202F and U+3000. With `\s`, the
//     anchored patterns below silently fail to match when a response begins
//     with NBSP or a Unicode space, so `"\u00a0<think>never closed"` kept its
//     `<think>` instead of stripping it — measured against the reference.
//   - Go's `$` matches only at the end of the text; Python's also matches
//     immediately before a TRAILING newline, which model output frequently has.
//
// Both were latent divergences in this file, surfaced by the reasoning
// differential harness because extract_think and extract_reasoning call
// StripThink.
var (
	// unclosedBlock: ^\s*<TAG>[\s\S]*$ — a streaming prefix never closed.
	unclosedBlock = regexp.MustCompile(`(?s)^` + pySpaceClass + `*<` + thinkingTagAlt + `>.*` + pyEnd)
	// selfClosingStart / selfClosingEnd: <thinking/> is an empty marker.
	selfClosingStart = regexp.MustCompile(`^` + pySpaceClass + `*<thinking/>` + pySpaceClass + `*`)
	selfClosingEnd   = regexp.MustCompile(pySpaceClass + `*<thinking/>` + pySpaceClass + `*` + pyEnd)
	// orphanCloseStart / orphanCloseEnd: closing tags with no opening one,
	// stripped only at the edges so prose discussing the tokens survives.
	orphanCloseStart = regexp.MustCompile(`^` + pySpaceClass + `*</` + thinkingTagAlt + `>` + pySpaceClass + `*`)
	orphanCloseEnd   = regexp.MustCompile(pySpaceClass + `*</` + thinkingTagAlt + `>` + pySpaceClass + `*` + pyEnd)
	// channelMarker: harmony-style markers, stripped only at the start.
	channelMarker = regexp.MustCompile(`^` + pySpaceClass + `*<\|?channel\|?>` + pySpaceClass + `*`)
	// partialTag: a control tag cut in half by a stream boundary.
	//
	// The reference's partial_control_tag has TWO alternatives and both are
	// needed: the thinking-tag prefixes, and a partial harmony channel marker
	// (`<|c`, `<|chan`, ...). Only the first was ported, so a response ending
	// `answer<|chan` kept the fragment instead of stripping it.
	partialTag = regexp.MustCompile(
		`(?:` + partialThinkingTag + `|<\|?(?:c|ch|cha|chan|chann|channe|channel)(?:\|?>?)?)` + pyEnd)
	// lonePipe: a dangling `<|` at the start.
	lonePipe = regexp.MustCompile(`^` + pySpaceClass + `*<\|?` + pyEnd)
)

// tagNameChars is the class the reference's negative lookahead rejects:
// [A-Za-z0-9_\-:>/]. A `<think` followed by one of these is part of a longer
// valid construct, not a malformed tag.
func isTagNameChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-' || c == ':' || c == '>' || c == '/':
		return true
	}
	return false
}

// stripMalformedOpenTags ports `<{_THINKING_TAG}(?![A-Za-z0-9_\-:>/])`.
//
// RE2 has no lookahead, so the scan is explicit. The alternation order matters
// and is preserved: at each position "think" is tried before "thinking", so for
// `<thinking广场` the first candidate fails its lookahead (the next character is
// `i`, a tag-name character) and the longer tag is then tried and matches.
//
// The class is deliberately ASCII-only. Python's `\w` was rejected for this
// step precisely because it also matches CJK, which would defeat the fix for
// `<think广场…` leaks; using Go's `\w` here would reintroduce that bug.
func stripMalformedOpenTags(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] != '<' {
			b.WriteByte(text[i])
			i++
			continue
		}
		matched := false
		for _, tag := range thinkingTags {
			end := i + 1 + len(tag)
			if end > len(text) || text[i+1:end] != tag {
				continue
			}
			// The lookahead succeeds at end of string.
			if end == len(text) || !isTagNameChar(text[end]) {
				i = end
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(text[i])
			i++
		}
	}
	return b.String()
}

// StripThink ports strip_think (utils/helpers.py:168).
//
// It removes well-formed thinking blocks, unclosed streaming prefixes,
// malformed opening tags, and tokenizer-level template leaks. It is applied
// before persisting to the history journal, so its guarantees hold for
// whatever consumes that journal later.
//
// Two steps are edge-only by design — the orphan closing tags and the harmony
// channel markers. Stripping those tokens mid-text would silently rewrite a
// message in which a user or the assistant discusses the tokens themselves.
func StripThink(text string) string {
	// Every supported control tag contains '<'; ordinary text only needs
	// trimming. This is both the reference's fast path and a correctness
	// guard for the malformed-tag scan below.
	if !strings.Contains(text, "<") {
		return PyStrip(text)
	}

	for _, re := range wellFormedTag {
		text = re.ReplaceAllString(text, "")
	}
	text = unclosedBlock.ReplaceAllString(text, "")
	text = selfClosingStart.ReplaceAllString(text, "")
	text = selfClosingEnd.ReplaceAllString(text, "")
	text = stripMalformedOpenTags(text)
	text = orphanCloseStart.ReplaceAllString(text, "")
	text = orphanCloseEnd.ReplaceAllString(text, "")
	text = channelMarker.ReplaceAllString(text, "")
	text = partialTag.ReplaceAllString(text, "")
	text = lonePipe.ReplaceAllString(text, "")
	return PyStrip(text)
}

// TruncatedSuffix is _TRUNCATED_SUFFIX (helpers.py:371). The exact text is part
// of the observable output, so it is spelled once here.
const TruncatedSuffix = "\n... (truncated)"

// TruncateText truncates to maxChars CHARACTERS, appending the reference's
// suffix. Mirrors truncate_text (helpers.py:402).
//
// The unit is characters, not bytes, and that distinction is load-bearing:
// Python's len() counts code points, so `truncate_text("日"*100, 50)` returns 50
// CJK characters plus the suffix — 66 characters, 166 bytes. A byte-based
// implementation returns 27 characters for the same call, which is a different
// answer, not merely a different encoding. Every caller of this function is
// reasoning in characters, so the port must too.
func TruncateText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + TruncatedSuffix
}
