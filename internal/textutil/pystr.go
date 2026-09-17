package textutil

import "unicode/utf8"

// Python string-semantics helpers.
//
// Go's strings.TrimSpace / unicode.IsSpace are NOT drop-in replacements for
// Python's str.strip() / str.rstrip(): the two definitions of "whitespace"
// differ. This file exists so the port can reproduce the reference exactly
// instead of approximately.
//
// pySpaceRanges is the exact set of code points for which Python's
// str.isspace() is true. It was derived by ENUMERATION from the reference
// interpreter, not read off a Unicode table by hand:
//
//	[cp for cp in range(0x110000) if chr(cp).isspace()]   # 29 code points
//
// Both interpreters available in this project (the system python3 and
// .tools/venv/bin/python) report 3.14.7 and produce this identical set.
//
// Relationship to Go: unicode.IsSpace covers the same set EXCEPT
// U+001C..U+001F (FILE/GROUP/RECORD/UNIT SEPARATOR). Python classifies those
// as whitespace because str.isspace() follows the Unicode bidirectional
// classes WS, B and S; Unicode's White_Space property does not include them.
// That four-code-point gap is the entire reason this table exists.
//
// Code points Python does NOT treat as whitespace and that must therefore
// survive trimming: U+200B (ZERO WIDTH SPACE), U+FEFF (BOM), U+180E.
var pySpaceRanges = [...]struct{ lo, hi rune }{
	{0x0009, 0x000D}, // TAB, LF, VT, FF, CR
	{0x001C, 0x0020}, // FILE/GROUP/RECORD/UNIT SEPARATOR, SPACE
	{0x0085, 0x0085}, // NEL
	{0x00A0, 0x00A0}, // NO-BREAK SPACE
	{0x1680, 0x1680}, // OGHAM SPACE MARK
	{0x2000, 0x200A}, // EN QUAD .. HAIR SPACE
	{0x2028, 0x2029}, // LINE SEPARATOR, PARAGRAPH SEPARATOR
	{0x202F, 0x202F}, // NARROW NO-BREAK SPACE
	{0x205F, 0x205F}, // MEDIUM MATHEMATICAL SPACE
	{0x3000, 0x3000}, // IDEOGRAPHIC SPACE
}

// PyIsSpace reports whether r is whitespace per Python's str.isspace().
//
// The ranges are sorted and non-overlapping, so this is a binary search.
func PyIsSpace(r rune) bool {
	lo, hi := 0, len(pySpaceRanges)-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		switch rng := pySpaceRanges[mid]; {
		case r < rng.lo:
			hi = mid - 1
		case r > rng.hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// PyRStrip is Python's str.rstrip() with no argument.
//
// Invalid UTF-8 is preserved rather than replaced: DecodeLastRuneInString
// reports RuneError for a malformed byte, RuneError is not whitespace, and
// trimming stops there. Python cannot reach this state at all (its text
// decoding fails first), so preserving the bytes is the least surprising
// choice for a Go string that may carry them.
func PyRStrip(s string) string {
	end := len(s)
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:end])
		if !PyIsSpace(r) {
			break
		}
		end -= size
	}
	return s[:end]
}

// PyLStrip is Python's str.lstrip() with no argument.
func PyLStrip(s string) string {
	start := 0
	for start < len(s) {
		r, size := utf8.DecodeRuneInString(s[start:])
		if !PyIsSpace(r) {
			break
		}
		start += size
	}
	return s[start:]
}

// PyStrip is Python's str.strip() with no argument.
func PyStrip(s string) string { return PyRStrip(PyLStrip(s)) }
