package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCaseInsensitiveSpanUsesByteOffsetsOfTheOriginal(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		query     string
		wantFound bool
		wantSlice string
	}{
		{"ascii", "Hello World", "world", true, "World"},
		{"accented", "café com leite", "CAFÉ", true, "café"},
		// U+212A KELVIN SIGN lowercases to 'k': one rune fewer in bytes, which
		// is what used to shift the offset of every later match.
		{"expanding rune before the match", "K grau e alvo", "alvo", true, "alvo"},
		{"missing", "nada aqui", "zzz", false, ""},
		{"empty query", "nada", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, found := caseInsensitiveSpan(tc.text, tc.query)
			if found != tc.wantFound {
				t.Fatalf("found=%v want %v", found, tc.wantFound)
			}
			if !found {
				return
			}
			if start < 0 || end > len(tc.text) || start > end {
				t.Fatalf("offsets out of range: %d..%d len=%d", start, end, len(tc.text))
			}
			got := tc.text[start:end]
			if !strings.EqualFold(got, tc.query) {
				t.Fatalf("slice %q does not fold to query %q", got, tc.query)
			}
			if tc.wantSlice != "" && got != tc.wantSlice {
				t.Fatalf("slice = %q, want %q", got, tc.wantSlice)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("slice is not valid UTF-8: %q", got)
			}
		})
	}
}

// The snippet window is taken in bytes; without snapping it starts and ends in
// the middle of a multi-byte rune and the JSON encoder replaces the fragments
// with U+FFFD.
func TestSearchSnippetWindowIsRuneAligned(t *testing.T) {
	// 200 accented characters before the match and 200 after, so both the -80
	// and the +160 byte edges land inside a 2-byte rune.
	text := strings.Repeat("á", 200) + " alvo " + strings.Repeat("é", 200)
	start, end, found := caseInsensitiveSpan(text, "alvo")
	if !found {
		t.Fatal("match not found")
	}
	snippet := text[snapRuneStart(text, start-80):snapRuneEnd(text, end+160)]
	if !utf8.ValidString(snippet) {
		t.Fatalf("snippet is not valid UTF-8: %q", snippet)
	}
	if strings.ContainsRune(snippet, utf8.RuneError) {
		t.Fatalf("snippet contains a replacement character: %q", snippet)
	}
	if !strings.Contains(snippet, "alvo") {
		t.Fatalf("snippet lost the match: %q", snippet)
	}
}

func TestSnapRuneBoundaries(t *testing.T) {
	text := "áé" // 4 bytes, 2 runes
	if got := snapRuneStart(text, 1); got != 0 {
		t.Fatalf("snapRuneStart mid-rune = %d, want 0", got)
	}
	if got := snapRuneStart(text, 2); got != 2 {
		t.Fatalf("snapRuneStart at boundary = %d, want 2", got)
	}
	if got := snapRuneEnd(text, 1); got != 2 {
		t.Fatalf("snapRuneEnd mid-rune = %d, want 2", got)
	}
	if got := snapRuneStart(text, -5); got != 0 {
		t.Fatalf("snapRuneStart negative = %d, want 0", got)
	}
	if got := snapRuneEnd(text, 99); got != len(text) {
		t.Fatalf("snapRuneEnd past the end = %d, want %d", got, len(text))
	}
}
