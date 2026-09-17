package textutil

import (
	"math"
	"strings"
	"unicode"
)

// PyAtoi parses s exactly as Python's `int(s.strip())` does.
//
// The expression, not `int(s)`, is the contract because that is what the
// reference writes at memory.py:376 and memory.py:501. The difference is real:
// CPython's int() skips a NARROWER whitespace set than str.strip() does, so
//
//	int("5\x1c")        -> ValueError
//	int("5\x1c".strip()) -> 5
//
// and it is the second form the reference relies on.
//
// Go's strconv.Atoi is not a substitute. Measured against Python 3.14:
//
//	"1_0"  -> Python 10,   strconv.Atoi fails (Python allows underscores between digits)
//	"١٢"   -> Python 12,   strconv.Atoi fails (Python accepts Unicode decimal digits)
//	"+5"   -> Python 5,    strconv.Atoi accepts this one
//	"5__0" -> Python ValueError, rejected here too
//
// ok is false wherever Python raises ValueError, with one deliberate exception
// noted below.
func PyAtoi(s string) (int, bool) {
	return parsePyIntBody(PyStrip(s))
}

// PyInt parses s exactly as Python's bare `int(s)` does, with no preceding
// str.strip().
//
// Use this, not PyAtoi, wherever the reference writes `int(value)` on a value
// that may be a string. The two differ only in the whitespace they tolerate,
// and that difference was measured rather than assumed: iterating every code
// point and keeping those for which both int(c+"5") and int("5"+c) equal 5
// yields exactly 25 characters —
//
//	U+0009 U+000A U+000B U+000C U+000D U+0020 U+0085 U+00A0
//	U+1680 U+2000..U+200A U+2028 U+2029 U+202F U+205F U+3000
//
// which is precisely Go's unicode.IsSpace, i.e. exactly what strings.TrimSpace
// removes. Python's str.strip() tolerates four characters more, U+001C..U+001F,
// so PyAtoi("5\x1c") is 5 while int("5\x1c") is ValueError in Python and
// PyInt("5\x1c") reports ok=false here.
func PyInt(s string) (int, bool) {
	return parsePyIntBody(strings.TrimSpace(s))
}

// parsePyIntBody implements the part of int() that does not depend on how the
// surrounding whitespace was removed.
func parsePyIntBody(body string) (int, bool) {
	if body == "" {
		return 0, false
	}

	negative := false
	switch body[0] {
	case '+':
		body = body[1:]
	case '-':
		negative = true
		body = body[1:]
	}
	if body == "" {
		return 0, false
	}

	// Python allows a single underscore BETWEEN digits and nowhere else, so
	// "_5", "5_" and "5__0" are all ValueError while "1_000_000" is 1000000.
	limit := uint64(math.MaxInt64)
	if negative {
		// The negative range has one more representable value than the
		// positive one, which is what makes math.MinInt64 reachable.
		limit = uint64(math.MaxInt64) + 1
	}

	var acc uint64
	sawDigit := false
	prevUnderscore := false
	for _, r := range body {
		if r == '_' {
			if !sawDigit || prevUnderscore {
				return 0, false
			}
			prevUnderscore = true
			continue
		}
		d, ok := decimalDigitValue(r)
		if !ok {
			return 0, false
		}
		if acc > (limit-uint64(d))/10 {
			// DELIBERATE DIVERGENCE: Python has arbitrary-precision integers,
			// so int("9" * 30) succeeds there and would be returned as a huge
			// cursor. Go's int is 64-bit, so a value outside the range is
			// reported as unparseable. Callers treat that as "no usable
			// cursor" (0 / None), which is the same direction Python's own
			// `cursor >= 0` guard fails in for garbage input, and a cursor of
			// 10^19 is unreachable from any real history file.
			return 0, false
		}
		acc = acc*10 + uint64(d)
		sawDigit = true
		prevUnderscore = false
	}
	if !sawDigit || prevUnderscore {
		return 0, false
	}

	if negative {
		if acc == uint64(math.MaxInt64)+1 {
			return math.MinInt64, true
		}
		return -int(acc), true
	}
	return int(acc), true
}

// decimalDigitValue returns the numeric value of a Unicode decimal digit.
//
// Python's int() accepts every character in the Nd category, not just ASCII, so
// "١٢" (U+0661 U+0662) is 12 and "５" (U+FF15) is 5. Scripts may be mixed:
// "5٥" is 55. Every Nd range is exactly ten consecutive code points, so the
// value is the offset from the range start.
func decimalDigitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	for _, rt := range unicode.Nd.R16 {
		if uint32(r) >= uint32(rt.Lo) && uint32(r) <= uint32(rt.Hi) {
			return int((uint32(r) - uint32(rt.Lo)) / uint32(rt.Stride)), true
		}
	}
	for _, rt := range unicode.Nd.R32 {
		if uint32(r) >= rt.Lo && uint32(r) <= rt.Hi {
			return int((uint32(r) - rt.Lo) / rt.Stride), true
		}
	}
	return 0, false
}
