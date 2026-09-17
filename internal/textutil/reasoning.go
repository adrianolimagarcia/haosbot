package textutil

import (
	"fmt"
	"regexp"
	"strings"
)

// Reasoning extraction, ported from nanobot/utils/helpers.py.
//
// These three functions are the single source of truth for "what reasoning did
// one model response carry, and what answer text remains after we peel it out".
// AgentRunner.run calls extract_reasoning on EVERY response (runner.py:467) and
// then, at runner.py:477-480, surfaces the reasoning to the hook unless it was
// already streamed.
//
// pySpaceClass is Python's `\s` as a regexp character class.
//
// Python's `\s` in a str pattern is EXACTLY str.isspace() — the same predicate
// PyStrip implements — and is NOT Go's `\s`, which RE2 defines as the ASCII set
// `[\t\n\f\r ]`. The two definitions disagree on U+000B, U+001C..U+001F
// (FILE/GROUP/RECORD/UNIT SEPARATOR), U+0085, U+00A0, U+1680, U+2000..U+200A,
// U+2028, U+2029, U+202F, U+205F and U+3000 — every one of which Python
// matches and Go does not. Writing `\s` in a pattern ported from the reference
// therefore silently changes the answer for any response containing NBSP or a
// Unicode space, which real model output does contain.
//
// The class is GENERATED from pySpaceRanges — the same table PyStrip uses —
// so the regexp and the trim can never drift apart.
var pySpaceClass = func() string {
	var b strings.Builder
	b.WriteByte('[')
	for _, rng := range pySpaceRanges {
		if rng.lo == rng.hi {
			fmt.Fprintf(&b, `\x{%x}`, rng.lo)
			continue
		}
		fmt.Fprintf(&b, `\x{%x}-\x{%x}`, rng.lo, rng.hi)
	}
	b.WriteByte(']')
	return b.String()
}()

// pyEnd is Python's `$` in a pattern compiled WITHOUT re.MULTILINE.
//
// Python's `$` matches at the end of the text OR immediately before a trailing
// newline; Go's `$` (without (?m)) matches only at the end of the text. The two
// therefore disagree on any input whose last character is a newline — which
// model output frequently is, and which the differential corpus exercises.
//
// `(?:\n?\z)` is the equivalent: match at the end, or consume one trailing
// newline and match there. Consuming the newline as part of the match rather
// than leaving it behind is unobservable here, because every caller of these
// patterns finishes with PyStrip, which removes it either way.
const pyEnd = `(?:\n?\z)`

var (
	// stripReasoningSelfCloseStart / End: a self-closing `<thinking/>` marker
	// is removed only at the edges.
	stripReasoningSelfCloseStart = regexp.MustCompile(
		`^` + pySpaceClass + `*<` + thinkingTagAlt + `/>` + pySpaceClass + `*`)
	stripReasoningSelfCloseEnd = regexp.MustCompile(
		pySpaceClass + `*<` + thinkingTagAlt + `/>` + pySpaceClass + `*` + pyEnd)
	// stripReasoningOpenStart: an opening wrapper at the very start.
	stripReasoningOpenStart = regexp.MustCompile(
		`^` + pySpaceClass + `*<` + thinkingTagAlt + `>` + pySpaceClass + `*`)
	// stripReasoningCloseEnd: a closing wrapper at the very end.
	stripReasoningCloseEnd = regexp.MustCompile(
		pySpaceClass + `*</` + thinkingTagAlt + `>` + pySpaceClass + `*` + pyEnd)
	// stripReasoningPartial: a control tag cut in half by a stream boundary.
	//
	// This rule is far more aggressive than its name suggests: the prefix
	// alternation includes the single characters `t` and `th`, so any text
	// ending in one of those is truncated ("answer t" -> "answer"). That is
	// the reference's behaviour, not an approximation of it.
	stripReasoningPartial = regexp.MustCompile(
		pySpaceClass + `*(?:` + partialThinkingTag + `)` + pyEnd)
)

// StripReasoningTags ports strip_reasoning_tags (helpers.py:226).
//
// It removes wrapper tags from text that is ALREADY known to be reasoning, and
// deliberately does less than StripThink:
//
//   - it never removes a well-formed block in the middle of the text, only the
//     opening wrapper at the start and the closing wrapper at the end;
//   - it has no malformed-opening-tag rule, so `<thinkx answer` survives;
//   - a leading orphan `</think>` survives (only a TRAILING one is removed).
//
// The parameter is `any`, mirroring the reference's `text: object`: a value
// that is not a str is not an error, it strips to "" (helpers.py:228-229).
// That guard is observable — a thinking block whose "thinking" is a number or
// a list contributes nothing rather than failing the whole response.
func StripReasoningTags(text any) string {
	s, ok := text.(string)
	if !ok {
		return ""
	}
	if !strings.Contains(s, "<") {
		return PyStrip(s)
	}
	s = stripReasoningSelfCloseStart.ReplaceAllString(s, "")
	s = stripReasoningSelfCloseEnd.ReplaceAllString(s, "")
	s = stripReasoningOpenStart.ReplaceAllString(s, "")
	s = stripReasoningCloseEnd.ReplaceAllString(s, "")
	s = stripReasoningPartial.ReplaceAllString(s, "")
	return PyStrip(s)
}

// inlineThinkParts collects the bodies of every CLOSED inline thinking block,
// in text order.
//
// The reference matches them with one pattern carrying a backreference,
// `<(?P<tag>think|thinking|thought)>([\s\S]*?)</(?P=tag)>`, which RE2 cannot
// express. Running three per-tag patterns instead would be wrong in a way that
// matters here but not in StripThink: finditer yields matches in TEXT order
// across all tag names, so a document holding `<thought>a</thought>` before
// `<think>b</think>` yields parts [a, b] — three separate passes would yield
// [b, a] and reverse the emitted reasoning. The scan below reproduces the
// leftmost-first, non-overlapping semantics directly.
//
// A failed attempt (an opening tag with no matching closing tag after it)
// advances one byte, which is what the regex engine does when it backtracks
// out of a start position.
func inlineThinkParts(text string) []string {
	// A closed block needs a closing tag, and every closing tag contains "</",
	// so text without one cannot hold a closed block. This is not an
	// approximation: the search below would fail on every candidate anyway.
	//
	// It also keeps the scan linear for the case that actually happens — a
	// streamed buffer holding many unclosed `<think>` prefixes and no closing
	// tag yet. Without the guard each unclosed prefix triggers a full failed
	// search for its closing tag, which is quadratic in the buffer length.
	if !strings.Contains(text, "</") {
		return nil
	}

	var parts []string
	for pos := 0; pos < len(text); {
		// Leftmost opening tag at or after pos, trying the alternation in the
		// reference's order at each position.
		openIdx, tag := -1, ""
		for i := pos; i < len(text); i++ {
			if text[i] != '<' {
				continue
			}
			for _, name := range thinkingTags {
				if strings.HasPrefix(text[i:], "<"+name+">") {
					openIdx, tag = i, name
					break
				}
			}
			if openIdx >= 0 {
				break
			}
		}
		if openIdx < 0 {
			return parts
		}
		bodyStart := openIdx + len(tag) + 2
		closeTag := "</" + tag + ">"
		rel := strings.Index(text[bodyStart:], closeTag)
		if rel < 0 {
			// No matching close tag: this start position cannot match.
			pos = openIdx + 1
			continue
		}
		closeIdx := bodyStart + rel
		parts = append(parts, PyStrip(text[bodyStart:closeIdx]))
		pos = closeIdx + len(closeTag)
	}
	return parts
}

// ExtractThink ports extract_think (helpers.py:240).
//
// It returns (thinking_text, cleaned_text). Only CLOSED blocks are surfaced;
// an unclosed streaming prefix is stripped from the cleaned text but not
// returned, because StripThink already handles that case.
//
// The thinking value is a *string because the reference distinguishes None
// from "": no closed block yields None, but a single EMPTY block yields ""
// (`"\n\n".join([""])` is "" and the `if parts` test is about the list, not
// about its contents). That difference is preserved here rather than collapsed.
func ExtractThink(text string) (*string, string) {
	// The fast path returns text.strip(), which is PyStrip — not StripThink.
	// The two agree here, because StripThink's own fast path is the same call.
	if !strings.Contains(text, "<") {
		return nil, PyStrip(text)
	}
	parts := inlineThinkParts(text)
	var thinking *string
	if len(parts) > 0 {
		joined := strings.Join(parts, "\n\n")
		thinking = &joined
	}
	return thinking, StripThink(text)
}

// stripContentOrSelf is `strip_think(content) if content else content`
// (helpers.py:313). Python truthiness, so both None and "" pass through
// unchanged and are NOT stripped to a fresh value.
func stripContentOrSelf(content *string) *string {
	if content == nil || *content == "" {
		return content
	}
	stripped := StripThink(*content)
	return &stripped
}

// ExtractReasoning ports extract_reasoning (helpers.py:292).
//
// It returns (reasoning_text, cleaned_content) from one model response, with
// the reference's three-tier fallback order:
//
//  1. dedicated `reasoning_content` (DeepSeek-R1, Kimi, MiMo, OpenAI reasoning
//     models, Bedrock);
//  2. Anthropic `thinking_blocks`;
//  3. inline `<think>` / `<thinking>` / `<thought>` blocks in `content`.
//
// Only ONE source contributes per response: lower-priority sources are IGNORED
// when a higher-priority one is present. Inline thinking tags are nonetheless
// always stripped from `content`, so they can never leak into the final answer.
//
// Two collapses from the reference are load-bearing and preserved exactly:
//
//   - the tier tests are PYTHON TRUTHINESS, not nil-ness, so `""` does not
//     select a tier (a response carrying `reasoning_content=""` falls through
//     to the next source);
//   - the thinking_blocks tier returns `joined or None`, so a tier that
//     selects but produces no usable text yields nil — while the inline tier
//     can legitimately yield "" for an empty block, because it returns
//     extract_think's value directly without that `or None`.
func ExtractReasoning(
	reasoningContent *string,
	thinkingBlocks []map[string]any,
	content *string,
) (*string, *string) {
	if reasoningContent != nil && *reasoningContent != "" {
		reasoning := StripReasoningTags(*reasoningContent)
		return &reasoning, stripContentOrSelf(content)
	}
	if len(thinkingBlocks) > 0 {
		var parts []string
		for _, block := range thinkingBlocks {
			// `tb.get("type") == "thinking"`. A non-string type is compared
			// as a string here rather than via `==` on `any`, so an
			// uncomparable dynamic type cannot panic.
			kind, _ := block["type"].(string)
			if kind != "thinking" {
				continue
			}
			if part := StripReasoningTags(block["thinking"]); part != "" {
				parts = append(parts, part)
			}
		}
		var reasoning *string
		if joined := strings.Join(parts, "\n\n"); joined != "" {
			reasoning = &joined
		}
		return reasoning, stripContentOrSelf(content)
	}
	if content != nil && *content != "" {
		// `return extract_think(content)` — note the cleaned value here is
		// extract_think's own strip_think output, and the reasoning value is
		// returned WITHOUT the `or None` collapse the thinking_blocks tier
		// applies, so an empty block legitimately yields "" rather than nil.
		thinking, cleaned := ExtractThink(*content)
		return thinking, &cleaned
	}
	return nil, content
}
