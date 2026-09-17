package memory

import (
	"testing"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// _normalize_history_entry (memory.py:259) decides oversize with
// `len(content) > limit`, which counts CHARACTERS. The port compared byte
// lengths instead.
//
// The persisted text was never wrong — TruncateText only cuts when the rune
// count exceeds the limit — but the one-shot oversize flag fired for any
// multi-byte entry whose byte length crossed the cap. These cases are the
// reference's own output, obtained by calling _normalize_history_entry on a real
// MemoryStore.
func TestNormalizeHistoryEntryOversizeCountsCharacters(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		limit      int
		wantFlag   bool
		wantRunes  int
		wantChange bool
	}{
		{"ascii-under", "abcde", 8, false, 5, false},
		{"ascii-over", "abcdefghij", 8, true, 24, true},

		// 5 runes / 10 bytes: byte counting would have flagged this.
		{"multibyte-under", "áéíóú", 8, false, 5, false},
		// 8 runes / 13 bytes: exactly at the rune limit, well past it in bytes.
		{"multibyte-equal", "áéíóúabc", 8, false, 8, false},
		// 3 runes / 9 bytes: the tightest case, one byte over and three runes under.
		{"cjk-under", "日本語", 8, false, 3, false},
		// 2 runes / 8 bytes: byte length equals the limit, so both agree.
		{"emoji-under", "🙂🙂", 8, false, 2, false},

		// Genuinely over the rune limit: truncated AND flagged.
		{"multibyte-over", "áéíóúabcde", 8, true, 24, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewMemoryStore(t.TempDir(), 1000)
			if err != nil {
				t.Fatalf("NewMemoryStore: %v", err)
			}
			limit := tc.limit
			got := s.normalizeHistoryEntry(tc.content, &limit)

			if n := utf8.RuneCountInString(got); n != tc.wantRunes {
				t.Errorf("result has %d runes, want %d (%q)", n, tc.wantRunes, got)
			}
			if changed := got != tc.content; changed != tc.wantChange {
				t.Errorf("changed = %v, want %v", changed, tc.wantChange)
			}
			if s.oversizeLogged != tc.wantFlag {
				t.Errorf("oversizeLogged = %v, want %v (runes=%d bytes=%d limit=%d)",
					s.oversizeLogged, tc.wantFlag,
					utf8.RuneCountInString(tc.content), len(tc.content), tc.limit)
			}
		})
	}
}

// TestNormalizeHistoryEntryTruncationSuffix pins the documented quirk that the
// result EXCEEDS the limit by the length of the suffix, because the reference
// appends after cutting.
func TestNormalizeHistoryEntryTruncationSuffix(t *testing.T) {
	s, err := NewMemoryStore(t.TempDir(), 1000)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	limit := 8
	got := s.normalizeHistoryEntry("abcdefghij", &limit)
	if want := 8 + len([]rune(textutil.TruncatedSuffix)); utf8.RuneCountInString(got) != want {
		t.Errorf("result is %d runes, want %d (limit %d + suffix %d)",
			utf8.RuneCountInString(got), want, limit, len([]rune(textutil.TruncatedSuffix)))
	}
}
