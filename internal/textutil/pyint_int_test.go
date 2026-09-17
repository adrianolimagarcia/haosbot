package textutil

import (
	"strings"
	"testing"
	"unicode"
)

// pythonIntWhitespace is the exact set of code points that CPython's int()
// tolerates on either side of a numeric literal.
//
// GENERATED, not transcribed. Produced by running Python 3.14 and keeping every
// code point cp for which BOTH int(chr(cp)+"5") == 5 and int("5"+chr(cp)) == 5:
//
//	tol = []
//	for cp in range(0x110000):
//	    if 0xD800 <= cp <= 0xDFFF: continue
//	    c = chr(cp)
//	    try:
//	        if int(c+"5") == 5 and int("5"+c) == 5: tol.append(cp)
//	    except ValueError:
//	        pass
//
// The result is 25 code points. This is the empirical basis for PyInt using
// strings.TrimSpace: if Go's unicode.IsSpace ever covers a different set, PyInt
// silently stops matching Python, so the set is asserted exhaustively below
// rather than sampled.
var pythonIntWhitespace = []rune{
	0x0009, 0x000A, 0x000B, 0x000C, 0x000D, 0x0020,
	0x0085, 0x00A0, 0x1680,
	0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006,
	0x2007, 0x2008, 0x2009, 0x200A,
	0x2028, 0x2029, 0x202F, 0x205F, 0x3000,
}

// TestUnicodeIsSpaceEqualsPythonIntTolerance is the load-bearing test for
// PyInt. It walks every assignable code point and asserts that unicode.IsSpace
// — and therefore strings.TrimSpace — accepts exactly the set Python's int()
// accepts, with no extra and no missing character.
//
// If this fails, PyInt is wrong in one of two directions: it would accept a
// character Python rejects (parsing "5\u200b" as 5 instead of raising), or
// reject one Python accepts (parsing "5\u00a0" as unparseable instead of 5).
func TestUnicodeIsSpaceEqualsPythonIntTolerance(t *testing.T) {
	want := make(map[rune]bool, len(pythonIntWhitespace))
	for _, r := range pythonIntWhitespace {
		want[r] = true
	}

	var extra, missing []rune
	for cp := rune(0); cp <= 0x10FFFF; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue // surrogates are not assignable; unicode.IsSpace is false
		}
		got := unicode.IsSpace(cp)
		switch {
		case got && !want[cp]:
			extra = append(extra, cp)
		case !got && want[cp]:
			missing = append(missing, cp)
		}
	}

	if len(extra) > 0 {
		t.Errorf("unicode.IsSpace accepts %d code point(s) Python's int() rejects: %s",
			len(extra), formatRunes(extra))
	}
	if len(missing) > 0 {
		t.Errorf("unicode.IsSpace rejects %d code point(s) Python's int() accepts: %s",
			len(missing), formatRunes(missing))
	}

	// Guard against the table itself being edited to an empty or wrong list.
	if len(want) != 25 {
		t.Errorf("pythonIntWhitespace has %d entries, want 25", len(want))
	}
}

func formatRunes(rs []rune) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, "U+"+hex4(r))
	}
	return strings.Join(parts, " ")
}

func hex4(r rune) string {
	const digits = "0123456789ABCDEF"
	var buf [4]byte
	for i := 3; i >= 0; i-- {
		buf[i] = digits[r&0xF]
		r >>= 4
	}
	return string(buf[:])
}

// TestPyIntMatchesPython pins int(s) for the value shapes the reference feeds
// it. Every want was produced by running Python 3.14; none was inferred.
func TestPyIntMatchesPython(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		// Plain and whitespace-padded.
		{"150", 150, true},
		{" 150 ", 150, true},
		{"\t42\n", 42, true},
		{"  +7  ", 7, true},
		{"007", 7, true},
		{"-007", -7, true},
		{"-0", 0, true},
		{"+0", 0, true},

		// Underscores: allowed only BETWEEN digits.
		{"1_0", 10, true},
		{"1_000_000", 1000000, true},
		{"5__0", 0, false},
		{"_5", 0, false},
		{"5_", 0, false},
		{"1_", 0, false},
		{"_", 0, false},

		// Unicode decimal digits, including mixed scripts.
		{"\u0661\u0662", 12, true}, // Arabic-Indic ١٢
		{"\uff15", 5, true},        // Fullwidth ５
		{"5\u0665", 55, true},      // mixed ASCII + Arabic-Indic

		// Signs.
		{"+5", 5, true},
		{"-5", -5, true},
		{"+", 0, false},
		{"-", 0, false},
		{"+-5", 0, false},

		// Rejected forms.
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"1.5", 0, false},
		{"0x10", 0, false},
		{"1e3", 0, false},

		// Python int() tolerates these (str.strip() also does).
		{"5\u00a0", 5, true}, // NBSP
		{"\u00a05", 5, true},
		{"5\u3000", 5, true}, // ideographic space
		{"\u30005", 5, true},

		// Python int() REJECTS these, even though str.strip() removes them.
		// This is the whole reason PyInt and PyAtoi both exist.
		{"5\x1c", 0, false},
		{"\x1c5", 0, false},
		{"5\x1d", 0, false},
		{"5\x1e", 0, false},
		{"5\x1f", 0, false},

		// Not whitespace to either: zero-width space and BOM.
		{"\u200b5", 0, false},
		{"5\ufeff", 0, false},
	}

	for _, tc := range cases {
		got, ok := PyInt(tc.in)
		if ok != tc.ok {
			t.Errorf("PyInt(%q) ok = %v, want %v (Python: %s)",
				tc.in, ok, tc.ok, pyOutcome(tc))
			continue
		}
		if ok && got != tc.want {
			t.Errorf("PyInt(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func pyOutcome(tc struct {
	in   string
	want int
	ok   bool
}) string {
	if tc.ok {
		return "int() succeeded"
	}
	return "int() raised ValueError"
}

// TestPyIntAndPyAtoiDivergeOnFileSeparators pins the ONE behavioural difference
// between the two helpers, so that neither can be swapped for the other by
// accident.
//
// Python's str.strip() removes U+001C..U+001F; int() does not. The reference
// writes int(text.strip()) for the memory cursors (hence PyAtoi) and bare
// int(value) for provider usage counts (hence PyInt).
func TestPyIntAndPyAtoiDivergeOnFileSeparators(t *testing.T) {
	for _, sep := range []string{"\x1c", "\x1d", "\x1e", "\x1f"} {
		in := sep + "5"

		// Python: int("5\x1c") -> ValueError, int("5\x1c".strip()) -> 5
		if v, ok := PyInt(in); ok {
			t.Errorf("PyInt(%q) = %d, true; Python's int() raises ValueError", in, v)
		}
		if v, ok := PyAtoi(in); !ok || v != 5 {
			t.Errorf("PyAtoi(%q) = %d, %v; Python's int(s.strip()) is 5", in, v, ok)
		}
	}

	// For every other tolerated character the two must agree.
	for _, r := range pythonIntWhitespace {
		in := string(r) + "5"
		a, aok := PyInt(in)
		b, bok := PyAtoi(in)
		if a != b || aok != bok {
			t.Errorf("PyInt and PyAtoi disagree on %q: %d/%v vs %d/%v",
				in, a, aok, b, bok)
		}
	}
}
