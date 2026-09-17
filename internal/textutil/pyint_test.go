package textutil

import (
	"math"
	"testing"
)

// Every expected value below was produced by running Python 3.14:
//
//	int(s.strip())
//
// and recording the result, including which inputs raise ValueError. None is
// transcribed from documentation.

func TestPyAtoiAccepted(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// Plain and signed.
		{"5", 5},
		{"+5", 5},
		{"-5", -5},
		{"0", 0},
		{"-0", 0},
		{"+0", 0},
		{"007", 7},

		// Boundary of the int64 range.
		{"9223372036854775807", math.MaxInt64},
		{"-9223372036854775808", math.MinInt64},
		{"999999999999999999", 999999999999999999},

		// Python's str.strip() removes these; strconv.Atoi would not.
		{" 5 ", 5},
		{"\x1c5", 5},   // U+001C: stripped by .strip(), NOT by Go's TrimSpace
		{"5\x1c", 5},   // and int("5\x1c") alone would raise; the .strip() saves it
		{"\u00a05", 5}, // NO-BREAK SPACE
		{"5\u00a0", 5}, // trailing
		{"\u30005", 5}, // IDEOGRAPHIC SPACE
		{"5\u3000", 5}, // trailing
		{"\t7\n", 7},
		{"  -42  ", -42},

		// Underscores between digits — accepted by Python, rejected by Atoi.
		{"1_0", 10},
		{"5_0", 50},
		{"1_0_0", 100},
		{"1_2_3_4", 1234},
		{"1_000_000", 1000000},
		{"+1_0", 10},
		{"-1_0", -10},

		// Unicode decimal digits (Nd) — accepted by Python, rejected by Atoi.
		{"\u0661\u0662", 12},        // ARABIC-INDIC ١٢
		{"\u0665", 5},               // ٥
		{"\u0660\u0661", 1},         // ٠١ = 01 = 1
		{"\u0663", 3},               // ٣
		{"\u0664\u0665\u0666", 456}, // ٤٥٦
		{"\uff15", 5},               // FULLWIDTH ５
		{"5\u0665", 55},             // mixed scripts are allowed
		{"\u0661\u0662\u0663", 123}, // ١٢٣
		{"-\u0661\u0662", -12},
		{"+\u0661\u0662", 12},
	}
	for _, tc := range cases {
		got, ok := PyAtoi(tc.in)
		if !ok {
			t.Errorf("PyAtoi(%q) reported failure, want %d", tc.in, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("PyAtoi(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPyAtoiRejected(t *testing.T) {
	// Each of these raises ValueError in Python.
	cases := []string{
		"5__0",    // doubled underscore
		"_5",      // leading underscore
		"5_",      // trailing underscore
		"+_5",     // underscore after a sign
		"-_5",     // same, negative
		"5.0",     // int() is not float()
		"5e2",     // no exponent
		"",        // empty
		"  ",      // whitespace only
		"abc",     // not numeric
		"0x10",    // no hex without base=
		"\u22125", // U+2212 MINUS SIGN, not the ASCII hyphen
		"+",       // sign only
		"-",       // sign only
	}
	for _, in := range cases {
		if v, ok := PyAtoi(in); ok {
			t.Errorf("PyAtoi(%q) = %d, true; Python raises ValueError", in, v)
		}
	}
}

// TestPyAtoiOverflowIsDeliberateDivergence documents the one place this port
// cannot match the reference.
//
// Python has arbitrary-precision integers, so int("9"*30) succeeds. Go's int is
// 64-bit. Rather than silently wrapping — which would hand a caller a negative
// cursor that reads as "unusable" for a completely different reason — the value
// is reported as unparseable.
func TestPyAtoiOverflowIsDeliberateDivergence(t *testing.T) {
	overflow := []string{
		"9999999999999999999",  // MaxInt64 + 2
		"9223372036854775808",  // MaxInt64 + 1
		"-9223372036854775809", // MinInt64 - 1
		"999999999999999999999999999999",
	}
	for _, in := range overflow {
		if v, ok := PyAtoi(in); ok {
			t.Errorf("PyAtoi(%q) = %d, true; expected the documented overflow rejection", in, v)
		}
	}

	// The boundary values themselves must still be exact, not off by one.
	if v, ok := PyAtoi("9223372036854775807"); !ok || v != math.MaxInt64 {
		t.Errorf("MaxInt64 = %d, ok=%v", v, ok)
	}
	if v, ok := PyAtoi("-9223372036854775808"); !ok || v != math.MinInt64 {
		t.Errorf("MinInt64 = %d, ok=%v", v, ok)
	}
}

// TestPyAtoiIsNotStrconvAtoi proves the helper is load-bearing rather than a
// cosmetic wrapper: it must accept inputs strconv.Atoi rejects.
func TestPyAtoiIsNotStrconvAtoi(t *testing.T) {
	for _, in := range []string{"1_0", "\u0661\u0662", "\x1c5", "5\x1c"} {
		if _, ok := PyAtoi(in); !ok {
			t.Errorf("PyAtoi(%q) failed, but Python accepts int(%q.strip())", in, in)
		}
	}
}

func TestPyAtoiStripsPythonWhitespaceNotGoWhitespace(t *testing.T) {
	// This is the specific gap that made the old implementation wrong:
	// strconv.Atoi(strings.TrimSpace("5\x1c")) fails, because Go's
	// unicode.IsSpace does not report U+001C.
	if v, ok := PyAtoi("5\x1c"); !ok || v != 5 {
		t.Errorf(`PyAtoi("5\x1c") = %d, %v; want 5, true`, v, ok)
	}
	if v, ok := PyAtoi("\x1c\x1d\x1e\x1f5\x1f\x1e\x1d\x1c"); !ok || v != 5 {
		t.Errorf("PyAtoi with all four C0 separators = %d, %v; want 5, true", v, ok)
	}
}
