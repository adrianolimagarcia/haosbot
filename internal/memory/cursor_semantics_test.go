package memory

import (
	"os"
	"testing"
)

// The cursor files are read with `int(path.read_text().strip())` in the
// reference (memory.py:376 and memory.py:501). This port previously used
// strconv.Atoi(strings.TrimSpace(...)), which diverges on three separate axes.
// Every expected value below was produced by running the reference against a
// real workspace and writing the raw cursor file directly.
//
// Note the asymmetry the reference has and this test pins: the history cursor
// rejects negatives (`if cursor >= 0`), while get_last_dream_cursor has no such
// guard and happily returns -3.
func TestCursorParsingMatchesPythonReference(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantLast  int  // GetLatestCursor
		wantDream int  // GetLastDreamCursor
		parseable bool // _read_cursor_counter is not None
	}{
		{"canonical", "5", 5, 5, true},
		{"zero", "0", 0, 0, true},
		{"c0-leading", "\x1c5", 5, 5, true},
		{"c0-trailing", "5\x1c", 5, 5, true},
		{"c0-all-four", "\x1c\x1d\x1e\x1f7", 7, 7, true},
		{"underscores", "1_0", 10, 10, true},
		{"underscores-many", "1_0_0", 100, 100, true},
		{"arabic-indic", "\u0661\u0662", 12, 12, true},
		{"fullwidth", "\uff15", 5, 5, true},
		{"nbsp", "\u00a05", 5, 5, true},
		{"newline", "5\n", 5, 5, true},

		// Unparseable: ValueError in Python.
		{"doubled-underscore", "5__0", 0, 0, false},
		{"leading-underscore", "_5", 0, 0, false},
		{"trailing-underscore", "5_", 0, 0, false},
		{"garbage", "abc", 0, 0, false},
		{"float", "5.0", 0, 0, false},
		{"empty", "", 0, 0, false},

		// Negative: parsed, but the history cursor's `>= 0` guard rejects it
		// while the Dream cursor has no guard.
		{"negative", "-3", 0, -3, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			s, err := NewMemoryStore(ws, 1000)
			if err != nil {
				t.Fatalf("NewMemoryStore: %v", err)
			}
			_, _, _, _, cursorPath, dreamCursorPath := s.Paths()

			if err := os.WriteFile(cursorPath, []byte(tc.raw), 0o644); err != nil {
				t.Fatalf("write cursor: %v", err)
			}
			if err := os.WriteFile(dreamCursorPath, []byte(tc.raw), 0o644); err != nil {
				t.Fatalf("write dream cursor: %v", err)
			}

			if _, ok := s.readCursorCounter(); ok != tc.parseable {
				t.Errorf("readCursorCounter ok = %v, want %v (raw %q)", ok, tc.parseable, tc.raw)
			}

			gotLast, err := s.GetLatestCursor()
			if err != nil {
				t.Fatalf("GetLatestCursor: %v", err)
			}
			if gotLast != tc.wantLast {
				t.Errorf("GetLatestCursor = %d, want %d (raw %q)", gotLast, tc.wantLast, tc.raw)
			}

			gotDream, err := s.GetLastDreamCursor()
			if err != nil {
				t.Fatalf("GetLastDreamCursor: %v", err)
			}
			if gotDream != tc.wantDream {
				t.Errorf("GetLastDreamCursor = %d, want %d (raw %q)", gotDream, tc.wantDream, tc.raw)
			}
		})
	}
}

// TestCursorRoundTripIsUnchanged guards the write path, which must keep emitting
// canonical ASCII digits so the reader's wider grammar is only ever exercised by
// hand-edited files.
func TestCursorRoundTripIsUnchanged(t *testing.T) {
	ws := t.TempDir()
	s, err := NewMemoryStore(ws, 1000)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if err := s.SetLastDreamCursor(42); err != nil {
		t.Fatalf("SetLastDreamCursor: %v", err)
	}
	got, err := s.GetLastDreamCursor()
	if err != nil {
		t.Fatalf("GetLastDreamCursor: %v", err)
	}
	if got != 42 {
		t.Errorf("round trip = %d, want 42", got)
	}

	_, _, _, _, _, dreamCursorPath := s.Paths()
	raw, err := os.ReadFile(dreamCursorPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "42" {
		t.Errorf("persisted %q, want %q", raw, "42")
	}
}
