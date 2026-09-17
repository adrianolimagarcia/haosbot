package memory

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// These tests are the in-package half of the legacy HISTORY.md migration. They
// cover what the differential test in compat/ cannot reach from outside the
// package — the unexported helpers, the constructor wiring, and the "now"
// fallback arm — and they pin the behaviours a reader is most likely to
// "simplify" away.

// legacyWorkspace builds a workspace with the given legacy bytes and returns
// the workspace path and its memory directory.
func legacyWorkspace(t *testing.T, legacy []byte) (string, string) {
	t.Helper()
	ws := t.TempDir()
	memDir := filepath.Join(ws, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir memory: %v", err)
	}
	if legacy != nil {
		if err := os.WriteFile(filepath.Join(memDir, "HISTORY.md"), legacy, 0o644); err != nil {
			t.Fatalf("write legacy: %v", err)
		}
	}
	return ws, memDir
}

func legacyDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestMigrationRunsDuringConstruction is the headline behaviour: opening a
// workspace that still has the legacy file converts it.
func TestMigrationRunsDuringConstruction(t *testing.T) {
	ws, memDir := legacyWorkspace(t, []byte("[2024-01-01 10:00] a\n[2024-01-02 11:00] b\n"))

	s, err := NewMemoryStore(ws, DefaultMaxHistory)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}

	raw, err := os.ReadFile(s.historyFile)
	if err != nil {
		t.Fatalf("history.jsonl was not written: %v", err)
	}
	const want = `{"cursor": 1, "timestamp": "2024-01-01 10:00", "content": "a"}` + "\n" +
		`{"cursor": 2, "timestamp": "2024-01-02 11:00", "content": "b"}` + "\n"
	if string(raw) != want {
		t.Errorf("history.jsonl =\n%q\nwant\n%q", raw, want)
	}

	if got := legacyDirNames(t, memDir); !equalStrings(got,
		[]string{".cursor", ".dream_cursor", "HISTORY.md.bak", "history.jsonl"}) {
		t.Errorf("memory/ = %v", got)
	}

	// The legacy file is preserved, never deleted.
	backup, err := os.ReadFile(filepath.Join(memDir, "HISTORY.md.bak"))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if string(backup) != "[2024-01-01 10:00] a\n[2024-01-02 11:00] b\n" {
		t.Errorf("backup content = %q", backup)
	}
}

// TestMigrationAdvancesTheDreamCursorToo is the deliberate product decision
// called out in memory.py:132-134: an upgrade must not replay the user's entire
// historical archive into Dream on first start.
func TestMigrationAdvancesTheDreamCursorToo(t *testing.T) {
	ws, _ := legacyWorkspace(t, []byte("[2024-01-01 10:00] a\n[2024-01-02 11:00] b\n"))

	s, err := NewMemoryStore(ws, DefaultMaxHistory)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	cursor, err := os.ReadFile(s.cursorFile)
	if err != nil {
		t.Fatalf("cursor file: %v", err)
	}
	if string(cursor) != "2" {
		t.Errorf(".cursor = %q, want %q", cursor, "2")
	}
	dream, err := os.ReadFile(s.dreamCursorFile)
	if err != nil {
		t.Fatalf("dream cursor file: %v", err)
	}
	if string(dream) != "2" {
		t.Errorf(".dream_cursor = %q, want %q — the migration must default Dream to "+
			"already-processed", dream, "2")
	}
	if got, err := s.GetLastDreamCursor(); err != nil || got != 2 {
		t.Errorf("GetLastDreamCursor() = %d, %v; want 2, nil", got, err)
	}
	unprocessed, err := s.ReadUnprocessedHistory(2)
	if err != nil {
		t.Fatalf("ReadUnprocessedHistory: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Errorf("Dream would replay %d entries after an upgrade", len(unprocessed))
	}
}

// TestMigrationGuardsChangeNothing covers the two early returns. The legacy file
// must survive untouched in both.
func TestMigrationGuardsChangeNothing(t *testing.T) {
	t.Run("no_legacy_file", func(t *testing.T) {
		ws, memDir := legacyWorkspace(t, nil)
		if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
			t.Fatalf("NewMemoryStore: %v", err)
		}
		if got := legacyDirNames(t, memDir); len(got) != 0 {
			t.Errorf("a workspace with no HISTORY.md gained files: %v", got)
		}
	})

	t.Run("history_already_nonempty", func(t *testing.T) {
		ws, memDir := legacyWorkspace(t, []byte("[2024-01-01 10:00] a\n"))
		existing := []byte(`{"cursor": 9}` + "\n")
		if err := os.WriteFile(filepath.Join(memDir, "history.jsonl"), existing, 0o644); err != nil {
			t.Fatalf("seed history: %v", err)
		}

		if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
			t.Fatalf("NewMemoryStore: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(memDir, "history.jsonl"))
		if err != nil {
			t.Fatalf("history.jsonl: %v", err)
		}
		if string(got) != string(existing) {
			t.Errorf("history.jsonl was rewritten: %q", got)
		}
		if got := legacyDirNames(t, memDir); !equalStrings(got,
			[]string{"HISTORY.md", "history.jsonl"}) {
			t.Errorf("memory/ = %v, want the legacy file left in place", got)
		}
	})

	t.Run("history_exists_but_is_empty", func(t *testing.T) {
		// A zero-length file is not "already migrated": the guard is
		// `exists() and st_size > 0`, so an empty file still migrates.
		ws, memDir := legacyWorkspace(t, []byte("[2024-01-01 10:00] a\n"))
		if err := os.WriteFile(filepath.Join(memDir, "history.jsonl"), nil, 0o644); err != nil {
			t.Fatalf("seed history: %v", err)
		}
		if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
			t.Fatalf("NewMemoryStore: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(memDir, "history.jsonl"))
		if err != nil {
			t.Fatalf("history.jsonl: %v", err)
		}
		if !strings.Contains(string(got), `"content": "a"`) {
			t.Errorf("an empty history.jsonl should not block the migration: %q", got)
		}
	})
}

// TestMigrationOfEmptyLegacyStillBacksUp pins the shape that is easy to get
// wrong: an empty legacy file produces a backup but NO history.jsonl and NO
// cursor files, because _write_entries is skipped when there are no entries.
func TestMigrationOfEmptyLegacyStillBacksUp(t *testing.T) {
	ws, memDir := legacyWorkspace(t, []byte("   \n\t\n"))

	if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if got := legacyDirNames(t, memDir); !equalStrings(got, []string{"HISTORY.md.bak"}) {
		t.Errorf("memory/ = %v, want only HISTORY.md.bak", got)
	}
}

// TestMigrationBackupNumbering walks the .bak, .bak.2, .bak.3 sequence.
func TestMigrationBackupNumbering(t *testing.T) {
	for _, tc := range []struct {
		existing int
		want     string
	}{
		{0, "HISTORY.md.bak"},
		{1, "HISTORY.md.bak.2"},
		{2, "HISTORY.md.bak.3"},
	} {
		ws, memDir := legacyWorkspace(t, []byte("[2024-01-01 10:00] a\n"))
		for i := 0; i < tc.existing; i++ {
			name := "HISTORY.md.bak"
			if i > 0 {
				name = "HISTORY.md.bak." + itoa(i+1)
			}
			if err := os.WriteFile(filepath.Join(memDir, name), []byte("old"), 0o644); err != nil {
				t.Fatalf("seed backup: %v", err)
			}
		}
		if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
			t.Fatalf("NewMemoryStore: %v", err)
		}
		if _, err := os.Stat(filepath.Join(memDir, tc.want)); err != nil {
			t.Errorf("%d existing backups: %s missing: %v", tc.existing, tc.want, err)
		}
		// The pre-existing backups must not be overwritten.
		old, err := os.ReadFile(filepath.Join(memDir, "HISTORY.md.bak"))
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		if string(old) != "old" && tc.existing > 0 {
			t.Errorf("%d existing backups: HISTORY.md.bak was overwritten: %q", tc.existing, old)
		}
	}
}

// TestMigrationIsIdempotent verifies a second construction does not re-migrate:
// after the first run there is no legacy file, and a new backup must not appear.
func TestMigrationIsIdempotent(t *testing.T) {
	ws, memDir := legacyWorkspace(t, []byte("[2024-01-01 10:00] a\n"))

	for i := 0; i < 3; i++ {
		if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
			t.Fatalf("NewMemoryStore #%d: %v", i, err)
		}
	}
	if got := legacyDirNames(t, memDir); !equalStrings(got,
		[]string{".cursor", ".dream_cursor", "HISTORY.md.bak", "history.jsonl"}) {
		t.Errorf("memory/ = %v; repeated construction created extra files", got)
	}
}

// TestMigrationWhenLegacyIsADirectory covers the unreadable-file arm: the guard
// only checks existence, the read fails, and nothing at all changes.
func TestMigrationWhenLegacyIsADirectory(t *testing.T) {
	ws, memDir := legacyWorkspace(t, nil)
	if err := os.MkdirAll(filepath.Join(memDir, "HISTORY.md"), 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if _, err := NewMemoryStore(ws, DefaultMaxHistory); err != nil {
		t.Fatalf("NewMemoryStore must not fail on an unreadable legacy path: %v", err)
	}
	if got := legacyDirNames(t, memDir); !equalStrings(got, []string{"HISTORY.md"}) {
		t.Errorf("memory/ = %v, want the directory left alone", got)
	}
}

// TestMigrationDecodesInvalidUTF8 pins errors="replace" end to end.
//
// The interesting input is a truncated but VALID prefix: b"\xf0\x9f" is one
// U+FFFD in the reference, but a byte-at-a-time replacement loop would emit two.
func TestMigrationDecodesInvalidUTF8(t *testing.T) {
	ws, _ := legacyWorkspace(t, append([]byte("[2024-01-01 10:00] a"), 0xf0, 0x9f, 'b', '\n'))

	s, err := NewMemoryStore(ws, DefaultMaxHistory)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	raw, err := os.ReadFile(s.historyFile)
	if err != nil {
		t.Fatalf("history.jsonl: %v", err)
	}
	if !strings.Contains(string(raw), `"content": "a`+"\uFFFD"+`b"`) {
		t.Errorf("truncated prefix did not decode to exactly one U+FFFD: %q", raw)
	}
}

// TestDecodeLegacyUTF8MatchesCPython is the direct table for the replacement
// rule. Every expected value was produced by running
// b.decode("utf-8", errors="replace") in the reference interpreter.
func TestDecodeLegacyUTF8MatchesCPython(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"ascii", []byte("plain"), "plain"},
		{"two_lone_bytes", []byte{0xff, 0xff}, "\uFFFD\uFFFD"},
		{"lone_byte_between", []byte{'a', 0xff, 'b'}, "a\uFFFDb"},
		{"truncated_2byte", []byte{0xc3}, "\uFFFD"},
		{"truncated_3byte", []byte{0xe2, 0x82}, "\uFFFD"},
		{"truncated_4byte", []byte{0xf0, 0x9f}, "\uFFFD"},
		{"truncated_4byte_partial", []byte{0xf0, 0x9f, 0x98}, "\uFFFD"},
		{"second_byte_out_of_range", []byte{0xe0, 0x80}, "\uFFFD\uFFFD"},
		{"overlong_c0", []byte{0xc0, 0x80}, "\uFFFD\uFFFD"},
		{"overlong_c1", []byte{0xc1, 0xbf}, "\uFFFD\uFFFD"},
		{"surrogate", []byte{0xed, 0xa0, 0x80}, "\uFFFD\uFFFD\uFFFD"},
		{"above_max", []byte{0xf4, 0x90, 0x80, 0x80}, "\uFFFD\uFFFD\uFFFD\uFFFD"},
		{"stray_continuations", []byte{0x80, 0x80, 0x80}, "\uFFFD\uFFFD\uFFFD"},
		{"invalid_second_byte", []byte{0xc3, '('}, "\uFFFD("},
		{"f5_lead", []byte{0xf5, 0x80, 0x80, 0x80}, "\uFFFD\uFFFD\uFFFD\uFFFD"},
		{"truncated_after_valid", []byte{0xe0, 0xa0}, "\uFFFD"},
		{"truncated_surrogate_prefix", []byte{0xed, 0x9f}, "\uFFFD"},
		{"truncated_max_prefix", []byte{0xf4, 0x8f, 0xbf}, "\uFFFD"},
		{"valid_multibyte", []byte("caf\u00e9"), "caf\u00e9"},
		{"valid_replacement_char", []byte{0xef, 0xbf, 0xbd}, "\uFFFD"},
		{"valid_then_invalid", []byte{0xc3, 0xa9, 0xff}, "\u00e9\uFFFD"},
	}
	for _, tc := range cases {
		if got := decodeLegacyUTF8(tc.in); got != tc.want {
			t.Errorf("%s: decodeLegacyUTF8(% x) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestLegacyFallbackTimestampUsesNowWhenStatFails covers the OSError arm of
// memory.py:216-217, which the differential test cannot reach because the guard
// at memory.py:112 makes it a race in practice.
func TestLegacyFallbackTimestampUsesNowWhenStatFails(t *testing.T) {
	s := newStore(t)
	before := time.Now().Format(historyStampLayout)
	got := s.LegacyFallbackTimestamp()
	after := time.Now().Format(historyStampLayout)

	if got != before && got != after {
		t.Errorf("LegacyFallbackTimestamp() = %q, want the current local minute (%q..%q)",
			got, before, after)
	}
}

// TestFormatPyLocalStampRoundsLikeCPython pins the sub-second rounding.
//
// datetime.fromtimestamp rounds the timestamp to microseconds with
// round-half-even BEFORE formatting, so ...:59.9999995 becomes the NEXT minute.
// Go's Time.Format truncates and would say :59. A FixedZone keeps this
// independent of the machine's timezone.
func TestFormatPyLocalStampRoundsLikeCPython(t *testing.T) {
	loc := time.FixedZone("TEST", -3*3600)
	cases := []struct {
		name string
		ns   int64
		want string
	}{
		{"exact_second", 1_600_000_020_000_000_000, "2020-09-13 09:27"},
		{"tie_rounds_up_across_the_minute", 1_600_000_019_999_999_523, "2020-09-13 09:27"},
		{"just_below_the_tie", 1_600_000_019_999_999_284, "2020-09-13 09:26"},
		{"microsecond", 1_600_000_020_000_000_953, "2020-09-13 09:27"},
	}
	for _, tc := range cases {
		got := formatPyLocalStamp(time.Unix(0, tc.ns).In(loc))
		if got != tc.want {
			t.Errorf("%s (ns=%d): formatPyLocalStamp = %q, want %q", tc.name, tc.ns, got, tc.want)
		}
	}
}

// TestParseLegacyHistoryReturnsOneEntryPerChunk documents the invariant the
// differential test relies on: cursors are 1-based and contiguous.
func TestParseLegacyHistoryReturnsOneEntryPerChunk(t *testing.T) {
	s := newStore(t)
	entries := s.ParseLegacyHistory("[2024-01-01 10:00] a\n\nplain\n[2024-01-02 11:00] b")
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %#v", len(entries), entries)
	}
	for i, e := range entries {
		if e.Cursor != i+1 {
			t.Errorf("entry %d has cursor %d", i, e.Cursor)
		}
		if e.SessionKey != nil {
			t.Errorf("entry %d carries a session key", i)
		}
	}
	if entries[1].Content != "plain" {
		t.Errorf("chunk without a timestamp was rewritten: %q", entries[1].Content)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
