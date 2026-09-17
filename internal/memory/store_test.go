package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *MemoryStore {
	t.Helper()
	s, err := NewMemoryStore(t.TempDir(), DefaultMaxHistory)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	return s
}

// readHistory returns the raw lines of history.jsonl.
func readHistory(t *testing.T, s *MemoryStore) []string {
	t.Helper()
	_, hist, _, _, _, _ := s.Paths()
	b, err := os.ReadFile(hist)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read history: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestMemoryDirectoryIsCreatedEagerly pins that construction creates memory/,
// matching __init__ which calls ensure_dir rather than waiting for a write.
func TestMemoryDirectoryIsCreatedEagerly(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewMemoryStore(dir, DefaultMaxHistory); err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "memory")); err != nil || !fi.IsDir() {
		t.Errorf("memory/ was not created: %v", err)
	}
}

// TestHistoryRecordShape pins the on-disk record exactly. The timestamp format
// is the easiest thing to get wrong: it is naive local with minute precision,
// not ISO-8601.
func TestHistoryRecordShape(t *testing.T) {
	s := newStore(t)
	if _, err := s.AppendHistory("first entry", nil, ""); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	lines := readHistory(t, s)
	if len(lines) != 1 {
		t.Fatalf("history has %d lines, want 1", len(lines))
	}

	var rec map[string]any
	dec := json.NewDecoder(strings.NewReader(lines[0]))
	dec.UseNumber()
	if err := dec.Decode(&rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := rec["session_key"]; ok {
		t.Error("session_key must be absent when no session was supplied")
	}
	ts, _ := rec["timestamp"].(string)
	// %Y-%m-%d %H:%M — 16 characters, no seconds, no timezone, no T.
	if len(ts) != 16 || ts[4] != '-' || ts[10] != ' ' || ts[13] != ':' {
		t.Errorf("timestamp = %q, want the naive local %%Y-%%m-%%d %%H:%%M form", ts)
	}
	if strings.ContainsAny(ts, "TZ+") {
		t.Errorf("timestamp %q carries a timezone or ISO separator", ts)
	}
	if rec["content"] != "first entry" {
		t.Errorf("content = %v", rec["content"])
	}
	if rec["cursor"] != json.Number("1") {
		t.Errorf("first cursor = %v, want 1", rec["cursor"])
	}
}

// TestSessionKeyIsWrittenWhenSupplied verifies the optional field appears only
// when there is a session to attribute the entry to.
func TestSessionKeyIsWrittenWhenSupplied(t *testing.T) {
	s := newStore(t)
	if _, err := s.AppendHistory("with session", nil, "cli:1"); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	lines := readHistory(t, s)
	if !strings.Contains(lines[0], `"session_key": "cli:1"`) {
		t.Errorf("session_key missing or misordered: %s", lines[0])
	}
	// Key order is the reference's insertion order, and the separators are
	// json.dumps' defaults: ", " between items and ": " after a key. A compact
	// line still parses, so only a byte comparison with the reference catches
	// this — see docs/spec-memory.md §2.
	if !strings.HasPrefix(lines[0], `{"cursor": 1, "timestamp": `) {
		t.Errorf("key order or separators differ from the reference: %s", lines[0])
	}
}

// TestCursorsAreMonotonic verifies successive appends increment.
func TestCursorsAreMonotonic(t *testing.T) {
	s := newStore(t)
	for i := 1; i <= 5; i++ {
		got, err := s.AppendHistory("entry", nil, "")
		if err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
		if got != i {
			t.Errorf("cursor %d, want %d", got, i)
		}
	}
}

// TestCursorSurvivesHistoryLoss verifies the persisted counter keeps cursors
// monotonic even if the journal is truncated externally — otherwise a Dream
// cursor would re-consume entries it had already processed.
func TestCursorSurvivesHistoryLoss(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.AppendHistory("e", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, hist, _, _, _, _ := s.Paths()
	if err := os.WriteFile(hist, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.AppendHistory("after wipe", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("cursor after wipe = %d, want 4 (the counter is authoritative)", got)
	}
}

// TestCursorRecoversFromCorruptCounter verifies an unreadable counter falls
// back to scanning the journal rather than restarting at 1.
func TestCursorRecoversFromCorruptCounter(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.AppendHistory("e", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, _, cursorFile, _ := s.Paths()
	if err := os.WriteFile(cursorFile, []byte("not a number"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.AppendHistory("after corruption", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("cursor = %d, want 4 (scan the journal and take the max)", got)
	}
}

// TestStripThinkIsAppliedBeforePersisting verifies the journal never receives
// thinking-tag leaks, because Dream reads it back as model input.
func TestStripThinkIsAppliedBeforePersisting(t *testing.T) {
	s := newStore(t)
	if _, err := s.AppendHistory("<think>secret reasoning</think>visible answer", nil, ""); err != nil {
		t.Fatal(err)
	}
	lines := readHistory(t, s)
	if strings.Contains(lines[0], "secret reasoning") {
		t.Errorf("thinking block was persisted: %s", lines[0])
	}
	if !strings.Contains(lines[0], "visible answer") {
		t.Errorf("the visible answer was lost: %s", lines[0])
	}
}

// TestEntryStrippedToEmptyIsPersistedEmpty pins the subtle case: when stripping
// empties a non-empty entry, the empty string is stored rather than the raw
// text. Falling back to the raw would undo strip_think exactly where it matters.
func TestEntryStrippedToEmptyIsPersistedEmpty(t *testing.T) {
	s := newStore(t)
	if _, err := s.AppendHistory("<think>only reasoning, nothing else</think>", nil, ""); err != nil {
		t.Fatal(err)
	}
	lines := readHistory(t, s)
	if !strings.Contains(lines[0], `"content": ""`) {
		t.Errorf("expected empty content, got: %s", lines[0])
	}
}

// TestOversizeEntryIsTruncated verifies the emergency cap applies.
func TestOversizeEntryIsTruncated(t *testing.T) {
	s := newStore(t)
	big := strings.Repeat("Z", historyEntryHardCap+1000)
	if _, err := s.AppendHistory(big, nil, ""); err != nil {
		t.Fatal(err)
	}
	lines := readHistory(t, s)
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	content, _ := rec["content"].(string)
	if len([]rune(content)) > historyEntryHardCap+len("\n... (truncated)") {
		t.Errorf("content is %d chars, expected the cap to apply", len([]rune(content)))
	}
}

// TestCustomMaxCharsOverridesTheCap verifies the per-call limit wins.
func TestCustomMaxCharsOverridesTheCap(t *testing.T) {
	s := newStore(t)
	limit := 10
	if _, err := s.AppendHistory(strings.Repeat("Z", 100), &limit, ""); err != nil {
		t.Fatal(err)
	}
	lines := readHistory(t, s)
	if !strings.Contains(lines[0], "... (truncated)") {
		t.Errorf("the per-call cap was not applied: %s", lines[0])
	}
}

// TestMalformedLinesAreSkippedNotFatal verifies one bad line does not make the
// journal unreadable, matching _read_entries which skips unparseable lines.
func TestMalformedLinesAreSkippedNotFatal(t *testing.T) {
	s := newStore(t)
	if _, err := s.AppendHistory("good one", nil, ""); err != nil {
		t.Fatal(err)
	}
	_, hist, _, _, _, _ := s.Paths()
	f, err := os.OpenFile(hist, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{ this is not json\n")
	f.Close()
	if _, err := s.AppendHistory("good two", nil, ""); err != nil {
		t.Fatal(err)
	}

	valid, err := s.IterValidEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 2 {
		t.Errorf("valid entries = %d, want 2 (the malformed line must be skipped)", len(valid))
	}
}

// TestInvalidCursorsAreDropped covers the cursor validation rules, including
// the bool case that Python needs an explicit check for.
func TestInvalidCursorsAreDropped(t *testing.T) {
	s := newStore(t)
	_, hist, _, _, _, _ := s.Paths()
	lines := []string{
		`{"cursor":1,"timestamp":"2026-01-01 00:00","content":"ok"}`,
		`{"cursor":true,"timestamp":"2026-01-01 00:00","content":"bool cursor"}`,
		`{"cursor":-5,"timestamp":"2026-01-01 00:00","content":"negative"}`,
		`{"cursor":"7","timestamp":"2026-01-01 00:00","content":"string cursor"}`,
		`{"timestamp":"2026-01-01 00:00","content":"no cursor"}`,
		`{"cursor":2,"content":"no timestamp"}`,
		`{"cursor":3,"timestamp":"2026-01-01 00:00","content":"ok too"}`,
	}
	if err := os.WriteFile(hist, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valid, err := s.IterValidEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 2 {
		t.Fatalf("valid entries = %d, want 2", len(valid))
	}
	if valid[0].Cursor != 1 || valid[1].Cursor != 3 {
		t.Errorf("cursors = %d, %d; want 1, 3", valid[0].Cursor, valid[1].Cursor)
	}
}

// TestGetMemoryContextOmitsEmptyHeading verifies an empty memory file produces
// an empty string rather than a heading with nothing under it.
func TestGetMemoryContextOmitsEmptyHeading(t *testing.T) {
	s := newStore(t)
	got, err := s.GetMemoryContext()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("empty memory produced %q, want empty", got)
	}
	if err := s.WriteMemory("user likes Go"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetMemoryContext()
	if err != nil {
		t.Fatal(err)
	}
	if got != "## Long-term Memory\nuser likes Go" {
		t.Errorf("context = %q", got)
	}
}

// TestCompactionKeepsUnprocessedEntries is the subtle part of compaction: the
// retention limit yields to the Dream cursor, so a backlog Dream has not read
// survives instead of being silently discarded.
func TestCompactionKeepsUnprocessedEntries(t *testing.T) {
	s, err := NewMemoryStore(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.AppendHistory("entry", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Dream has consumed nothing, so nothing may be dropped.
	if err := s.CompactHistory(); err != nil {
		t.Fatal(err)
	}
	if got := len(readHistory(t, s)); got != 10 {
		t.Errorf("entries after compaction = %d, want 10 (none processed yet)", got)
	}

	// Once Dream catches up to cursor 8, only the unprocessed tail plus the
	// retention window must survive.
	if err := s.SetLastDreamCursor(8); err != nil {
		t.Fatal(err)
	}
	if err := s.CompactHistory(); err != nil {
		t.Fatal(err)
	}
	kept := readHistory(t, s)
	if len(kept) != 3 {
		t.Errorf("entries after compaction = %d, want 3", len(kept))
	}
	if !strings.Contains(kept[0], `"cursor": 8`) {
		t.Errorf("compaction kept the wrong tail: %s", kept[0])
	}
}

// TestCompactionIsNoOpUnderTheLimit verifies a small journal is untouched.
func TestCompactionIsNoOpUnderTheLimit(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendHistory("e", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	before := readHistory(t, s)
	if err := s.CompactHistory(); err != nil {
		t.Fatal(err)
	}
	after := readHistory(t, s)
	if len(before) != len(after) {
		t.Errorf("compaction changed a journal under the limit: %d -> %d", len(before), len(after))
	}
}

// TestDreamCursorRoundTrip verifies the Dream cursor persists, and that an
// absent file reads as 0 rather than an error.
func TestDreamCursorRoundTrip(t *testing.T) {
	s := newStore(t)
	got, err := s.GetLastDreamCursor()
	if err != nil {
		t.Fatalf("absent cursor: %v", err)
	}
	if got != 0 {
		t.Errorf("absent dream cursor = %d, want 0", got)
	}
	if err := s.SetLastDreamCursor(42); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetLastDreamCursor()
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("dream cursor = %d, want 42", got)
	}
}

// TestReadUnprocessedHistoryFiltersByCursor verifies the Dream input selection.
func TestReadUnprocessedHistoryFiltersByCursor(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendHistory("e", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ReadUnprocessedHistory(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unprocessed = %d, want 2 (cursors 4 and 5)", len(got))
	}
}

// TestUnicodeRoundTripsUnescaped verifies the journal matches Python's
// ensure_ascii=False: literal characters, not \uXXXX escapes.
func TestUnicodeRoundTripsUnescaped(t *testing.T) {
	s := newStore(t)
	content := "unicode \u2713 \u65e5\u672c \u2028 sep"
	if _, err := s.AppendHistory(content, nil, ""); err != nil {
		t.Fatal(err)
	}
	lines := readHistory(t, s)
	if strings.Contains(lines[0], `\u2713`) || strings.Contains(lines[0], `\u2028`) {
		t.Errorf("content was escaped; Python leaves it literal: %s", lines[0])
	}
	// And it must decode back to exactly what went in.
	valid, err := s.IterValidEntries()
	if err != nil {
		t.Fatal(err)
	}
	if got := valid[0].Entry["content"]; got != content {
		t.Errorf("round trip changed the content: %q", got)
	}
}
