package gitstore

import (
	"strings"
	"unicode"
)

// This file ports the handful of Python text/path primitives whose exact
// semantics are observable through the public API. They are written out rather
// than approximated with the Go standard library because the reference's output
// is compared byte for byte by other subsystems (docs/spec-memory.md:107).

// splitLinesStr mirrors str.splitlines() — the no-argument form used by
// GitStore.summarize_working_tree (gitstore.py:368-369).
//
// Python splits on more than "\n": \r, \r\n, \v, \f, \x1c, \x1d, \x1e, \x85,
// U+2028 and U+2029 are all line boundaries for str, and "\r\n" counts as one
// break. strings.Split(s, "\n") is not the same function.
func splitLinesStr(s string) []string {
	return splitLines(s, true)
}

// splitLinesBytes mirrors bytes.splitlines() — used for blob contents by
// dulwich (dulwich/objects.py:942 keeps the terminators; patch.py:353 drops
// them). For bytes Python does NOT treat \x85/U+2028/U+2029 as breaks, so the
// boundary set differs from the str one; see splitLines.
func splitLinesBytes(s string) []string {
	return splitLines(s, false)
}

// splitLinesKeepEnds mirrors str.splitlines(True)/bytes.splitlines(True):
// each returned line keeps its terminator. dulwich's Blob.splitlines() uses the
// keepends form so the diff writer can detect a missing final newline and emit
// "\ No newline at end of file" (dulwich/objects.py:942-965, patch.py:213).
func splitLinesKeepEnds(s string, isStr bool) []string {
	return splitLinesMode(s, isStr, true)
}

func splitLines(s string, isStr bool) []string {
	return splitLinesMode(s, isStr, false)
}

func splitLinesMode(s string, isStr, keepEnds bool) []string {
	var out []string
	start := 0
	i := 0
	for i < len(s) {
		c := s[i]
		var width int
		switch {
		case c == '\n' || c == '\v' || c == '\f' || c == 0x1c || c == 0x1d || c == 0x1e:
			width = 1
		case c == '\r':
			width = 1
			if i+1 < len(s) && s[i+1] == '\n' {
				width = 2
			}
		case isStr && c == 0xc2 && i+1 < len(s) && s[i+1] == 0x85:
			// UTF-8 encoded U+0085 (NEL) — a line boundary for str only.
			width = 2
		case isStr && c == 0xe2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xa8 || s[i+2] == 0xa9):
			// U+2028 LINE SEPARATOR / U+2029 PARAGRAPH SEPARATOR.
			width = 3
		default:
			i++
			continue
		}
		if keepEnds {
			out = append(out, s[start:i+width])
		} else {
			out = append(out, s[start:i])
		}
		i += width
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// decodeUTF8Replace mirrors bytes.decode("utf-8", errors="replace"), which the
// reference uses both for blob contents read out of the object store
// (gitstore.py:526) and for the working-tree file (gitstore.py:352).
//
// Go's utf8.DecodeRune replaces one byte at a time; CPython follows the Unicode
// "maximal subpart" rule, so a truncated 3-byte sequence yields ONE U+FFFD
// rather than three. The distinction is observable: a non-UTF-8 file produces a
// diff whose text is the decoded string, and GitStore's own binary-file branch
// is chosen by whether the decode raised (gitstore.py:356).
func decodeUTF8Replace(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	i := 0
	for i < len(b) {
		c := b[i]
		if c < 0x80 {
			sb.WriteByte(c)
			i++
			continue
		}
		size, ok := utf8SequenceLen(b[i:])
		if !ok {
			sb.WriteRune(unicode.ReplacementChar)
			i += size
			continue
		}
		sb.Write(b[i : i+size])
		i += size
	}
	return sb.String()
}

// utf8SequenceLen returns the length of the maximal subpart starting at b[0]
// and whether that subpart is a complete, valid UTF-8 sequence.
//
// The length is the number of bytes CPython's decoder consumes before emitting
// one U+FFFD, per Unicode 15 table 3-7.
func utf8SequenceLen(b []byte) (int, bool) {
	c := b[0]
	switch {
	case c < 0x80:
		return 1, true
	case c < 0xc2: // continuation byte or overlong lead
		return 1, false
	case c < 0xe0:
		if len(b) < 2 {
			return 1, false
		}
		if b[1] < 0x80 || b[1] > 0xbf {
			return 1, false
		}
		return 2, true
	case c < 0xf0:
		var lo, hi byte = 0x80, 0xbf
		switch {
		case c == 0xe0:
			lo = 0xa0
		case c == 0xed:
			hi = 0x9f
		}
		if len(b) < 2 {
			return 1, false
		}
		if b[1] < lo || b[1] > hi {
			return 1, false
		}
		if len(b) < 3 {
			return 2, false
		}
		if b[2] < 0x80 || b[2] > 0xbf {
			return 2, false
		}
		return 3, true
	case c < 0xf5:
		var lo, hi byte = 0x80, 0xbf
		switch {
		case c == 0xf0:
			lo = 0x90
		case c == 0xf4:
			hi = 0x8f
		}
		if len(b) < 2 {
			return 1, false
		}
		if b[1] < lo || b[1] > hi {
			return 1, false
		}
		if len(b) < 3 {
			return 2, false
		}
		if b[2] < 0x80 || b[2] > 0xbf {
			return 2, false
		}
		if len(b) < 4 {
			return 3, false
		}
		if b[3] < 0x80 || b[3] > 0xbf {
			return 3, false
		}
		return 4, true
	default:
		return 1, false
	}
}

// pythonStrip mirrors str.strip() with no argument, which removes every
// character for which str.isspace() is true. The reference applies it to commit
// messages in log (gitstore.py:263) and revert (gitstore.py:472-475).
//
// strings.TrimSpace is close but not equal: Python also strips the C0
// separators U+001C..U+001F, which Go's unicode.IsSpace does not report.
func pythonStrip(s string) string {
	return strings.TrimFunc(s, isPythonSpace)
}

func isPythonSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0:
		return true
	}
	return unicode.Is(unicode.Zs, r) || r == 0x2028 || r == 0x2029
}

// pythonStartsWith mirrors str.startswith for the plain-prefix form used by
// log's message filter and revert's prefix guard (gitstore.py:264, :476).
func pythonStartsWith(s, prefix string) bool { return strings.HasPrefix(s, prefix) }
