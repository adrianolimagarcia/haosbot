package textutil

import (
	"strings"
	"testing"
)

// TestPySpaceTableHasReferenceSize pins the table at the size the reference
// interpreter actually reports: 29 code points for which str.isspace() is true.
//
// The number is asserted rather than the ranges because a hand-edited range
// could keep the count while moving a boundary; the exhaustive comparison
// against the live reference in compat/differential_test.go is what pins the
// boundaries. This test fails fast, without a venv, when someone edits the
// table by hand.
func TestPySpaceTableHasReferenceSize(t *testing.T) {
	count := 0
	for _, rng := range pySpaceRanges {
		count += int(rng.hi-rng.lo) + 1
	}
	if count != 29 {
		t.Fatalf("pySpaceRanges covers %d code points, reference has 29", count)
	}
	if len(pySpaceRanges) != 10 {
		t.Errorf("pySpaceRanges has %d ranges, expected 10", len(pySpaceRanges))
	}
	// Sorted and non-overlapping is a precondition of the binary search.
	for i := 1; i < len(pySpaceRanges); i++ {
		if pySpaceRanges[i].lo <= pySpaceRanges[i-1].hi {
			t.Fatalf("ranges %d and %d overlap or are unsorted", i-1, i)
		}
	}
}

// TestPyStripDiffersFromStringsTrimSpace documents why this package exists.
//
// Go's unicode.IsSpace follows Unicode's White_Space property; Python's
// str.isspace() additionally accepts the bidirectional class B/S code points
// U+001C..U+001F. So strings.TrimSpace is NOT a drop-in for str.strip(), and a
// port that uses it diverges on exactly these four characters.
//
// If this test ever starts failing because strings.TrimSpace was changed, the
// Go helper is still correct — the reference is Python, not Go's stdlib.
func TestPyStripDiffersFromStringsTrimSpace(t *testing.T) {
	for _, r := range []rune{0x1C, 0x1D, 0x1E, 0x1F} {
		s := "x" + string(r)
		if got := strings.TrimSpace(s); got != s {
			t.Errorf("strings.TrimSpace(U+%04X) = %q; expected Go to leave it", r, got)
		}
		if got := PyStrip(s); got != "x" {
			t.Errorf("PyStrip(U+%04X) = %q, want %q", r, got, "x")
		}
		if !PyIsSpace(r) {
			t.Errorf("PyIsSpace(U+%04X) = false, Python says true", r)
		}
	}
	// The reverse direction: these are NOT whitespace in Python and must be
	// preserved even though they look like it.
	for _, r := range []rune{0x200B, 0xFEFF, 0x180E} {
		s := "x" + string(r)
		if got := PyStrip(s); got != s {
			t.Errorf("PyStrip(U+%04X) = %q, want it preserved as %q", r, got, s)
		}
	}
}

func TestPyStripBasics(t *testing.T) {
	cases := []struct {
		name, in, strip, rstrip, lstrip string
	}{
		{"plain", "abc", "abc", "abc", "abc"},
		{"empty", "", "", "", ""},
		{"ascii", "  abc \t\n", "abc", "  abc", "abc \t\n"},
		{"all ascii ws", " \t\n\r\v\f ", "", "", ""},
		{"nbsp", "\u00a0abc\u00a0", "abc", "\u00a0abc", "abc\u00a0"},
		{"ideographic", "\u3000abc\u3000", "abc", "\u3000abc", "abc\u3000"},
		{"line sep", "\u2028abc\u2029", "abc", "\u2028abc", "abc\u2029"},
		{"nel", "\u0085abc", "abc", "\u0085abc", "abc"},
		{"ogham", "\u1680abc", "abc", "\u1680abc", "abc"},
		{"en quad", "\u2000abc\u200a", "abc", "\u2000abc", "abc\u200a"},
		{"narrow nbsp", "\u202fabc\u205f", "abc", "\u202fabc", "abc\u205f"},
		{"cjk content kept", " \u65e5\u672c\u8a9e ", "\u65e5\u672c\u8a9e", " \u65e5\u672c\u8a9e", "\u65e5\u672c\u8a9e "},
		{"emoji kept", " \U0001f600 ", "\U0001f600", " \U0001f600", "\U0001f600 "},
		{"inner ws kept", " a b ", "a b", " a b", "a b "},
		{"zwsp kept", "\u200babc\u200b", "\u200babc\u200b", "\u200babc\u200b", "\u200babc\u200b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PyStrip(tc.in); got != tc.strip {
				t.Errorf("PyStrip(%q) = %q, want %q", tc.in, got, tc.strip)
			}
			if got := PyRStrip(tc.in); got != tc.rstrip {
				t.Errorf("PyRStrip(%q) = %q, want %q", tc.in, got, tc.rstrip)
			}
			if got := PyLStrip(tc.in); got != tc.lstrip {
				t.Errorf("PyLStrip(%q) = %q, want %q", tc.in, got, tc.lstrip)
			}
		})
	}
}

// TestPyStripPreservesInvalidUTF8 pins the chosen behavior for input Python
// cannot produce: a Go string carrying malformed bytes. Trimming must stop at
// the bad byte rather than replacing or dropping it, so no data is invented.
func TestPyStripPreservesInvalidUTF8(t *testing.T) {
	in := "  \xff\xfe  "
	got := PyStrip(in)
	if got != "\xff\xfe" {
		t.Fatalf("PyStrip(%q) = %q, want %q", in, got, "\xff\xfe")
	}
	// Trailing invalid bytes block the trim of the whitespace before them.
	in2 := "x \xff "
	if got := PyRStrip(in2); got != "x \xff" {
		t.Fatalf("PyRStrip(%q) = %q, want %q", in2, got, "x \xff")
	}
}

// TestStripThinkTrimsPythonWhitespace is the regression test for the bug this
// package was introduced to fix: StripThink used strings.TrimSpace, so an entry
// padded with U+001C..U+001F kept its padding while the reference removed it.
func TestStripThinkTrimsPythonWhitespace(t *testing.T) {
	for _, r := range []rune{0x1C, 0x1D, 0x1E, 0x1F, 0x00A0, 0x3000, 0x2028} {
		in := string(r) + "visible" + string(r)
		if got := StripThink(in); got != "visible" {
			t.Errorf("StripThink(%q) = %q, want %q", in, got, "visible")
		}
	}
	// The no-'<' fast path and the regex path must agree on trimming.
	withTag := string(rune(0x1C)) + "<think>x</think>visible" + string(rune(0x1F))
	if got := StripThink(withTag); got != "visible" {
		t.Errorf("StripThink(%q) = %q, want %q", withTag, got, "visible")
	}
}
