package telegram

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Unicode tables the reference obtains from Python's unicodedata / re modules.
//
// Go's standard library has no East Asian Width property at all, and its
// unicode tables are one Unicode version behind the reference interpreter
// (Go 1.23.5 ships Unicode 15.0; .tools/venv/bin/python is CPython 3.14.7 with
// unicodedata 16.0.0). Every table below was produced by ENUMERATING the
// reference interpreter, not by reading a Unicode data file:
//
//	[cp for cp in range(0x110000) if unicodedata.east_asian_width(chr(cp)) in ('W','F')]
//	[cp for cp in range(0x110000) if chr(cp).isdigit()]
//	[cp for cp in range(0x110000) if re.match(r"\w", chr(cp))]
//	[cp for cp in range(0x110000) if chr(cp).isspace()]
//
// compat/python/dump_telegram.py re-runs those enumerations and
// telegram_unicode_test.go checks the Go predicates against the dumped ranges
// for all 1,114,112 code points, so a table typo cannot pass unnoticed.

// runeRange is an inclusive code point range.
type runeRange struct{ lo, hi rune }

// inRanges reports whether r falls in a sorted, non-overlapping range table.
func inRanges(table []runeRange, r rune) bool {
	i := sort.Search(len(table), func(i int) bool { return table[i].hi >= r })
	return i < len(table) && r >= table[i].lo
}

// eastAsianWideRanges is unicodedata.east_asian_width(c) in ('W', 'F').
//
// The reference's `dw` counts each of these as TWO columns and everything else
// as one. Note that the two trailing ranges cover the whole CJK Extension B and
// later planes, including unassigned code points: UAX #11 assigns them the
// default width W, and Python reports that default.
var eastAsianWideRanges = []runeRange{
	{0x1100, 0x115F},
	{0x231A, 0x231B},
	{0x2329, 0x232A},
	{0x23E9, 0x23EC},
	{0x23F0, 0x23F0},
	{0x23F3, 0x23F3},
	{0x25FD, 0x25FE},
	{0x2614, 0x2615},
	{0x2630, 0x2637},
	{0x2648, 0x2653},
	{0x267F, 0x267F},
	{0x268A, 0x268F},
	{0x2693, 0x2693},
	{0x26A1, 0x26A1},
	{0x26AA, 0x26AB},
	{0x26BD, 0x26BE},
	{0x26C4, 0x26C5},
	{0x26CE, 0x26CE},
	{0x26D4, 0x26D4},
	{0x26EA, 0x26EA},
	{0x26F2, 0x26F3},
	{0x26F5, 0x26F5},
	{0x26FA, 0x26FA},
	{0x26FD, 0x26FD},
	{0x2705, 0x2705},
	{0x270A, 0x270B},
	{0x2728, 0x2728},
	{0x274C, 0x274C},
	{0x274E, 0x274E},
	{0x2753, 0x2755},
	{0x2757, 0x2757},
	{0x2795, 0x2797},
	{0x27B0, 0x27B0},
	{0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50},
	{0x2B55, 0x2B55},
	{0x2E80, 0x2E99},
	{0x2E9B, 0x2EF3},
	{0x2F00, 0x2FD5},
	{0x2FF0, 0x303E},
	{0x3041, 0x3096},
	{0x3099, 0x30FF},
	{0x3105, 0x312F},
	{0x3131, 0x318E},
	{0x3190, 0x31E5},
	{0x31EF, 0x321E},
	{0x3220, 0x3247},
	{0x3250, 0xA48C},
	{0xA490, 0xA4C6},
	{0xA960, 0xA97C},
	{0xAC00, 0xD7A3},
	{0xF900, 0xFAFF},
	{0xFE10, 0xFE19},
	{0xFE30, 0xFE52},
	{0xFE54, 0xFE66},
	{0xFE68, 0xFE6B},
	{0xFF01, 0xFF60},
	{0xFFE0, 0xFFE6},
	{0x16FE0, 0x16FE4},
	{0x16FF0, 0x16FF1},
	{0x17000, 0x187F7},
	{0x18800, 0x18CD5},
	{0x18CFF, 0x18D08},
	{0x1AFF0, 0x1AFF3},
	{0x1AFF5, 0x1AFFB},
	{0x1AFFD, 0x1AFFE},
	{0x1B000, 0x1B122},
	{0x1B132, 0x1B132},
	{0x1B150, 0x1B152},
	{0x1B155, 0x1B155},
	{0x1B164, 0x1B167},
	{0x1B170, 0x1B2FB},
	{0x1D300, 0x1D356},
	{0x1D360, 0x1D376},
	{0x1F004, 0x1F004},
	{0x1F0CF, 0x1F0CF},
	{0x1F18E, 0x1F18E},
	{0x1F191, 0x1F19A},
	{0x1F200, 0x1F202},
	{0x1F210, 0x1F23B},
	{0x1F240, 0x1F248},
	{0x1F250, 0x1F251},
	{0x1F260, 0x1F265},
	{0x1F300, 0x1F320},
	{0x1F32D, 0x1F335},
	{0x1F337, 0x1F37C},
	{0x1F37E, 0x1F393},
	{0x1F3A0, 0x1F3CA},
	{0x1F3CF, 0x1F3D3},
	{0x1F3E0, 0x1F3F0},
	{0x1F3F4, 0x1F3F4},
	{0x1F3F8, 0x1F43E},
	{0x1F440, 0x1F440},
	{0x1F442, 0x1F4FC},
	{0x1F4FF, 0x1F53D},
	{0x1F54B, 0x1F54E},
	{0x1F550, 0x1F567},
	{0x1F57A, 0x1F57A},
	{0x1F595, 0x1F596},
	{0x1F5A4, 0x1F5A4},
	{0x1F5FB, 0x1F64F},
	{0x1F680, 0x1F6C5},
	{0x1F6CC, 0x1F6CC},
	{0x1F6D0, 0x1F6D2},
	{0x1F6D5, 0x1F6D7},
	{0x1F6DC, 0x1F6DF},
	{0x1F6EB, 0x1F6EC},
	{0x1F6F4, 0x1F6FC},
	{0x1F7E0, 0x1F7EB},
	{0x1F7F0, 0x1F7F0},
	{0x1F90C, 0x1F93A},
	{0x1F93C, 0x1F945},
	{0x1F947, 0x1F9FF},
	{0x1FA70, 0x1FA7C},
	{0x1FA80, 0x1FA89},
	{0x1FA8F, 0x1FAC6},
	{0x1FACE, 0x1FADC},
	{0x1FADF, 0x1FAE9},
	{0x1FAF0, 0x1FAF8},
	{0x20000, 0x2FFFD},
	{0x30000, 0x3FFFD},
}

// eastAsianWide reports whether c occupies two columns in the reference's `dw`.
func eastAsianWide(r rune) bool { return inRanges(eastAsianWideRanges, r) }

// IsEastAsianWide reports whether unicodedata.east_asian_width(c) is "W" or "F"
// for c, which is the test _render_table_box's `dw` uses. It is exported because
// the embedded table is the one place this package knowingly diverges from Go's
// own Unicode data (Go 1.23 ships 15.0, the reference runs 16.0.0), so a caller
// auditing that table needs to reach it.
func IsEastAsianWide(r rune) bool { return eastAsianWide(r) }

// extraWordRanges is the part of Python's Unicode `\w` that Go 1.23's
// unicode.IsLetter/IsNumber do NOT cover: 5,004 code points added in Unicode 16.
//
// Derived by enumerating the reference's `re.match(r"\w", chr(cp))` and
// subtracting `unicode.IsLetter(cp) || unicode.IsNumber(cp) || cp == '_'`. The
// subtraction is EXACTLY empty in the other direction — Go never reports a word
// character Python does not — so a union is both sufficient and safe if a later
// Go release adds these ranges to its own tables.
var extraWordRanges = []runeRange{
	{0x1C89, 0x1C8A},
	{0xA7CB, 0xA7CD},
	{0xA7DA, 0xA7DC},
	{0x105C0, 0x105F3},
	{0x10D40, 0x10D65},
	{0x10D6F, 0x10D85},
	{0x10EC2, 0x10EC4},
	{0x11380, 0x11389},
	{0x1138B, 0x1138B},
	{0x1138E, 0x1138E},
	{0x11390, 0x113B5},
	{0x113B7, 0x113B7},
	{0x113D1, 0x113D1},
	{0x113D3, 0x113D3},
	{0x116D0, 0x116E3},
	{0x11BC0, 0x11BE0},
	{0x11BF0, 0x11BF9},
	{0x13460, 0x143FA},
	{0x16100, 0x1611D},
	{0x16130, 0x16139},
	{0x16D40, 0x16D6C},
	{0x16D70, 0x16D79},
	{0x18CFF, 0x18CFF},
	{0x1CCF0, 0x1CCF9},
	{0x1E5D0, 0x1E5ED},
	{0x1E5F0, 0x1E5FA},
	{0x2EBF0, 0x2EE5D},
}

// pyIsWord reproduces Python's Unicode `\w` for a str pattern.
//
// CPython defines it as Py_UNICODE_ISALNUM(ch) || ch == '_', and
// Py_UNICODE_ISALNUM is ISALPHA || ISDECIMAL || ISDIGIT || ISNUMERIC — that is,
// the Unicode categories L*, Nd, Nl and No, which is Go's IsLetter ∪ IsNumber.
// Go's `\w` is ASCII-only, so it cannot be used for a pattern translated from
// the reference.
func pyIsWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || inRanges(extraWordRanges, r)
}

// PyIsWordRune reports whether Python's `\w` matches r in a str pattern, which
// is `r.isalnum() or r == "_"` over the reference interpreter's Unicode tables.
func PyIsWordRune(r rune) bool { return pyIsWord(r) }

// extraDigitRanges is the part of Python's str.isdigit() that Go's
// unicode.IsDigit does NOT cover.
//
// str.isdigit() is true for every character whose Unicode Numeric_Type is
// Decimal OR Digit, so it includes ², ① and ⒈ — which are category No, not Nd.
// Go's IsDigit is Nd only. As with the word table, the difference in the other
// direction is empty.
var extraDigitRanges = []runeRange{
	{0xB2, 0xB3},
	{0xB9, 0xB9},
	{0x1369, 0x1371},
	{0x19DA, 0x19DA},
	{0x2070, 0x2070},
	{0x2074, 0x2079},
	{0x2080, 0x2089},
	{0x2460, 0x2468},
	{0x2474, 0x247C},
	{0x2488, 0x2490},
	{0x24EA, 0x24EA},
	{0x24F5, 0x24FD},
	{0x24FF, 0x24FF},
	{0x2776, 0x277E},
	{0x2780, 0x2788},
	{0x278A, 0x2792},
	{0x10A40, 0x10A43},
	{0x10D40, 0x10D49},
	{0x10E60, 0x10E68},
	{0x11052, 0x1105A},
	{0x116D0, 0x116E3},
	{0x11BF0, 0x11BF9},
	{0x16130, 0x16139},
	{0x16D70, 0x16D79},
	{0x1CCF0, 0x1CCF9},
	{0x1E5F1, 0x1E5FA},
	{0x1F100, 0x1F10A},
}

// pyIsDigit reproduces Python's str.isdigit().
func pyIsDigit(r rune) bool { return unicode.IsDigit(r) || inRanges(extraDigitRanges, r) }

// PyIsDigitRune reports whether Python's str.isdigit() is true for r. It is
// broader than unicode.IsDigit: "²", "①" and "⒈" have Numeric_Type=Digit and are
// accepted by the legacy `id|username` sender check.
func PyIsDigitRune(r rune) bool { return pyIsDigit(r) }

// pyIsDigitString reproduces `s.isdigit()`: true when s is non-empty and every
// character is a digit. The reference uses it on the numeric half of a legacy
// `id|username` sender string, so "²|alice" IS a legacy-shaped id in Python
// while Go's unicode.IsDigit would reject it.
func pyIsDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pyIsDigit(r) {
			return false
		}
	}
	return true
}

// pySpaceClass is the explicit RE2 character class equivalent to Python's `\s`
// in a str pattern.
//
// Python's `\s` for str patterns is exactly the 29 code points for which
// str.isspace() is true — verified by enumeration, and identical to the table
// in internal/textutil. Go's `\s` is the ASCII set [\t\n\f\r ] and is missing
// VT, the four separators U+001C..U+001F, NEL, NBSP, OGHAM SPACE MARK, the
// EN QUAD..HAIR SPACE block, LINE/PARAGRAPH SEPARATOR, NARROW NO-BREAK SPACE,
// MEDIUM MATHEMATICAL SPACE and IDEOGRAPHIC SPACE. Every pattern below that
// came from the reference with a `\s` uses this class instead.
const pySpaceClass = `[\x09-\x0d\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// pyWordClass is the explicit RE2 character class equivalent to Python's `\w`
// in a str pattern.
//
// Go's own `\w` is ASCII-only. `[\p{L}\p{N}_]` is very close but uses Go's
// Unicode tables, which are one version behind the reference's, so the 27
// Unicode-16 ranges that Go is missing are spliced in from extraWordRanges. The
// resulting class is exactly Python's `\w` for this reference, which
// telegram_unicode_test.go checks over all code points.
var pyWordClass = "[" + rangesToClass(extraWordRanges) + `\p{L}\p{N}_]`

// rangesToClass renders a range table as the body of an RE2 character class.
func rangesToClass(table []runeRange) string {
	var b strings.Builder
	for _, r := range table {
		if r.lo == r.hi {
			fmt.Fprintf(&b, `\x{%X}`, r.lo)
			continue
		}
		fmt.Fprintf(&b, `\x{%X}-\x{%X}`, r.lo, r.hi)
	}
	return b.String()
}
