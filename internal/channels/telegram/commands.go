package telegram

import (
	"regexp"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// CommandAlias is one entry of _TELEGRAM_COMMAND_ALIASES (runtime.py:468-473).
//
// Telegram's command menu only accepts [a-z0-9_], so the canonical hyphenated
// nanobot commands are registered under underscore aliases and mapped back
// before the core router sees them. The slice keeps the reference's insertion
// order, which _normalize_telegram_command iterates.
type CommandAlias struct {
	// Alias is the Telegram-safe spelling, e.g. "/dream_log".
	Alias string
	// Canonical is the nanobot spelling, e.g. "/dream-log".
	Canonical string
}

// CommandAliases is _TELEGRAM_COMMAND_ALIASES in insertion order.
var CommandAliases = []CommandAlias{
	{Alias: "/dream_log", Canonical: "/dream-log"},
	{Alias: "/dream_restore", Canonical: "/dream-restore"},
	{Alias: "/dream_prompt", Canonical: "/dream-prompt"},
	{Alias: "/evaluator_prompt", Canonical: "/evaluator-prompt"},
}

// displayCommandNames is the reference's `{value: key for key, value in ...}`.
var displayCommandNames = func() map[string]string {
	out := make(map[string]string, len(CommandAliases))
	for _, alias := range CommandAliases {
		out[alias.Canonical] = alias.Alias
	}
	return out
}()

// NormalizeCommand maps Telegram-safe command aliases back to canonical nanobot
// commands. Port of _normalize_telegram_command (runtime.py:594-602).
//
// Only an EXACT alias or an alias followed by a single space is rewritten, and
// the rewrite keeps the argument text: "/dream_log now" becomes
// "/dream-log now". "/dream_logx" is left alone. The argument slice is taken by
// CHARACTER index (Python `content[len(alias):]`), which matters because the
// argument text may be arbitrary Unicode.
func NormalizeCommand(content string) string {
	if !strings.HasPrefix(content, "/") {
		return content
	}
	for _, alias := range CommandAliases {
		if content == alias.Alias || strings.HasPrefix(content, alias.Alias+" ") {
			return alias.Canonical + string([]rune(content)[len([]rune(alias.Alias)):])
		}
	}
	return content
}

// ---------------------------------------------------------------------------
// Display-name rendering
// ---------------------------------------------------------------------------

// displayCommandLiterals is the alternation of _TELEGRAM_DISPLAY_COMMAND_RE.
// The alternatives share no prefix relationship that could make more than one
// of them match at the same position, which is what lets the hand-written
// scanner below ignore the engine's backtracking between alternatives.
var displayCommandLiterals = []string{
	"/dream-log",
	"/dream-restore",
	"/dream-prompt",
	"/evaluator-prompt",
}

// DisplayCommandReSource is the reference's pattern text, kept so a test can
// assert that the hand-written scanner was derived from it rather than from a
// paraphrase. It is NOT compiled: RE2 has no lookbehind and Go's `\w` is
// ASCII-only, so neither the original pattern nor a mechanical substitution of
// it is usable.
const DisplayCommandReSource = `(?<![\w/.-])/(?:dream-log|dream-restore|dream-prompt|evaluator-prompt)(?![\w/.-])`

// isDisplayCommandBoundary is the reference's `[\w/.-]` class.
//
// `\w` here is PYTHON's Unicode word class, not Go's ASCII one: the lookarounds
// exist so that "x/dream-log" and "/dream-logx" are not treated as commands, and
// a CJK or accented character next to the command must block the match exactly
// as a Latin letter would. The differential corpus covers both.
func isDisplayCommandBoundary(r rune) bool {
	return r == '/' || r == '.' || r == '-' || pyIsWord(r)
}

// matchDisplayCommandLiteral returns the alternative that matches line[i:], or
// "" when none does.
func matchDisplayCommandLiteral(runes []rune, i int) string {
	for _, literal := range displayCommandLiterals {
		if hasRunePrefix(runes[i:], literal) {
			return literal
		}
	}
	return ""
}

// displayCommandMatch is one match of the display-command pattern, in CHARACTER
// offsets.
type displayCommandMatch struct {
	start   int
	end     int
	literal string
}

// scanDisplayCommands is the engine behind FindDisplayCommands and
// ReplaceDisplayCommands. RE2 supports neither lookbehind nor lookahead, so the
// pattern
//
//	(?<![\w/.-])/(?:dream-log|dream-restore|dream-prompt|evaluator-prompt)(?![\w/.-])
//
// is reproduced as a left-to-right scan:
//
//   - a match at position i requires the preceding CHARACTER (not byte) to be
//     absent or outside the boundary class, the "/" at i, one of the four
//     literals, and the following character absent or outside the class;
//   - scanning resumes at i+1 after a failure and after the end of a match, and
//     replacement text is never rescanned.
//
// Equivalence was checked differentially against BOTH `findall` and `sub` on
// every case of the corpus, rather than by reading the pattern.
func scanDisplayCommands(runes []rune) []displayCommandMatch {
	var matches []displayCommandMatch
	i := 0
	for i < len(runes) {
		if runes[i] == '/' && (i == 0 || !isDisplayCommandBoundary(runes[i-1])) {
			if literal := matchDisplayCommandLiteral(runes, i); literal != "" {
				j := i + len([]rune(literal))
				if j == len(runes) || !isDisplayCommandBoundary(runes[j]) {
					matches = append(matches, displayCommandMatch{start: i, end: j, literal: literal})
					i = j
					continue
				}
			}
		}
		i++
	}
	return matches
}

// FindDisplayCommands returns the canonical commands the pattern matches in one
// LINE, in order. It exists so the scanner can be compared directly against
// Python's findall; ReplaceDisplayCommands is the production entry point.
func FindDisplayCommands(line string) []string {
	matches := scanDisplayCommands([]rune(line))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.literal)
	}
	return out
}

// ReplaceDisplayCommands rewrites canonical hyphenated commands to their
// Telegram-safe aliases. Port of the `_TELEGRAM_DISPLAY_COMMAND_RE.sub(...)`
// call inside _telegram_command_text (runtime.py:490).
func ReplaceDisplayCommands(line string) string {
	runes := []rune(line)
	matches := scanDisplayCommands(runes)
	if len(matches) == 0 {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	prev := 0
	for _, m := range matches {
		b.WriteString(string(runes[prev:m.start]))
		b.WriteString(displayCommandNames[m.literal])
		prev = m.end
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

// inlineCommandCodeRe is the second substitution in _telegram_command_text. It
// is ASCII-only and needs no rework: RE2 supports every construct it uses.
//
// The reference comment explains why it exists — inline code is not tappable as
// a Telegram command, so the alias substitution above must not apply inside it —
// but the substitution itself is a NO-OP on the backticks: group 1 is the
// command text and it replaces the whole match, so “ `/dream_log` “ becomes
// `/dream_log`. That is the reference's behaviour and it is reproduced here.
var inlineCommandCodeRe = regexp.MustCompile("`(/(?:dream_log|dream_restore|dream_prompt|evaluator_prompt)(?: [^`\\n]*)?)`")

// DisplayCommandText renders command references for Telegram without changing
// fenced code or diffs. Port of _telegram_command_text (runtime.py:476-501).
//
// Three details are load-bearing and easy to get wrong:
//
//   - the text is split with PYTHON's str.splitlines(keepends=True), whose
//     boundary set is {LF, VT, FF, CR, CRLF, U+001C, U+001D, U+001E, U+0085,
//     U+2028, U+2029} — NOT just "\n". A vertical tab therefore ends a line and
//     can open or close a fence;
//   - `line.lstrip()` is Python's lstrip, so NBSP and U+001C..U+001F are
//     stripped and U+200B is not;
//   - a fence closes on the same three-character marker it opened with, and the
//     marker is `stripped[:3]`, a CHARACTER slice.
func DisplayCommandText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	fence := ""
	hasFence := false
	for _, line := range pySplitLinesKeepEnds(text) {
		stripped := textutil.PyLStrip(line)
		switch {
		case hasFence:
			if strings.HasPrefix(stripped, fence) {
				hasFence = false
			}
		case strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~"):
			fence = string([]rune(stripped)[:3])
			hasFence = true
		default:
			line = ReplaceDisplayCommands(line)
			line = inlineCommandCodeRe.ReplaceAllString(line, "$1")
		}
		b.WriteString(line)
	}
	return b.String()
}

// pySplitLinesKeepEnds is Python's str.splitlines(keepends=True).
//
// The boundary set was obtained by enumerating the reference interpreter:
// LF, VT, FF, CR, CRLF (one boundary), U+001C, U+001D, U+001E, U+0085, U+2028
// and U+2029. U+001F and U+200B are NOT boundaries, and neither is U+000B's
// neighbour U+000A alone being special. Go's bufio.Scanner and strings.Split
// both split only on LF and cannot be used.
func pySplitLinesKeepEnds(s string) []string {
	if s == "" {
		return []string{}
	}
	runes := []rune(s)
	out := []string{}
	start := 0
	i := 0
	for i < len(runes) {
		if !isPyLineBoundary(runes[i]) {
			i++
			continue
		}
		end := i + 1
		if runes[i] == '\r' && end < len(runes) && runes[end] == '\n' {
			end++
		}
		out = append(out, string(runes[start:end]))
		start = end
		i = end
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

// isPyLineBoundary reports whether r ends a line for Python's str.splitlines.
func isPyLineBoundary(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1C, 0x1D, 0x1E, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Slash-command routing
// ---------------------------------------------------------------------------

// BusSlashCommandRe is TELEGRAM_BUS_SLASH_COMMAND_RE (runtime.py:534-539): the
// slash commands routed to AgentLoop through _forward_command. The canonical
// hyphenated commands are handled by a separate handler.
//
// RE2 REWORK, two changes:
//
//   - `\w` and `\s` become pyWordClass / pySpaceClass, because Go's are
//     ASCII-only while Python's str patterns are Unicode-aware. `@nanobot` with
//     a non-ASCII suffix is a different match in the two engines otherwise;
//   - a bare `$` becomes `\n?$`. Python's `$` without MULTILINE also matches
//     just before a trailing newline; RE2's matches only at the very end, so
//     "/new\n" would stop being recognised.
var BusSlashCommandRe = regexp.MustCompile(
	`^/(?:new|compact|stop|restart|status|dream|history|goal|trigger|pairing|model|skill` +
		`|dream_log|dream_restore|dream_prompt|evaluator_prompt|evaluator-prompt)` +
		`(?:@` + pyWordClass + `+)?(?:` + pySpaceClass + `+.*)?\n?$`)

// BusSlashCommandSource is the reference's pattern text for BusSlashCommandRe.
const BusSlashCommandSource = `^/(?:new|compact|stop|restart|status|dream|history|goal|trigger|pairing|model|skill|dream_log|dream_restore|dream_prompt|evaluator_prompt|evaluator-prompt)(?:@\w+)?(?:\s+.*)?$`
