package compat

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/memory"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_legacy_history.py) and its own loader. It shares no
// declarations with differential_test.go beyond repoRoot, because
// dump_reference.py and differential_test.go are edited concurrently by several
// agents and a read-modify-write race there would destroy work.
//
// Every expectation below comes from executing the frozen reference. Nothing is
// transcribed from documentation.

// legacyHistoryRef is the cached result of running the reference dumper.
type legacyHistoryRef struct {
	doc    map[string]any
	pinOff int
	skip   string
	err    error
}

var (
	legacyHistoryOnce sync.Once
	legacyHistoryVal  legacyHistoryRef
)

// legacyHistoryReference returns the reference document and the UTC offset the
// interpreter was pinned to, running the dumper at most once per test binary.
func legacyHistoryReference(t *testing.T) (map[string]any, int) {
	t.Helper()
	root := repoRoot(t)
	legacyHistoryOnce.Do(func() { legacyHistoryVal = loadLegacyHistoryReference(root) })

	switch {
	case legacyHistoryVal.skip != "":
		t.Skip(legacyHistoryVal.skip)
	case legacyHistoryVal.err != nil:
		t.Fatalf("%v", legacyHistoryVal.err)
	}
	return legacyHistoryVal.doc, legacyHistoryVal.pinOff
}

// loadLegacyHistoryReference runs the dumper twice.
//
// The first run discovers the mtimes the dumper pins on the legacy file; the
// second pins the interpreter's local zone to the offset Go's time.Local uses at
// those instants. _legacy_fallback_timestamp formats a local time, so without
// that pinning the comparison would depend on which zone the machine happens to
// be in rather than on the port being correct.
func loadLegacyHistoryReference(root string) legacyHistoryRef {
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_legacy_history.py")

	if _, err := os.Stat(python); err != nil {
		return legacyHistoryRef{skip: fmt.Sprintf(
			"SKIP: reference venv not present at %s — differential check not run", python)}
	}
	if _, err := os.Stat(script); err != nil {
		return legacyHistoryRef{skip: fmt.Sprintf("SKIP: dumper missing at %s", script)}
	}

	discovery, err := runLegacyHistoryDumper(python, script, root, nil)
	if err != nil {
		return legacyHistoryRef{err: err}
	}
	pin, err := legacyHistoryPinOffset(discovery)
	if err != nil {
		return legacyHistoryRef{err: err}
	}
	doc, err := runLegacyHistoryDumper(python, script, root, &pin)
	if err != nil {
		return legacyHistoryRef{err: err}
	}
	return legacyHistoryRef{doc: doc, pinOff: pin}
}

func runLegacyHistoryDumper(python, script, root string, offset *int) (map[string]any, error) {
	env := os.Environ()
	if offset != nil {
		env = append(env, fmt.Sprintf("LEGACY_HISTORY_UTC_OFFSET_SECONDS=%d", *offset))
	}
	out, err := runReferenceCommand("legacy-history dumper", []string{python, script}, root, env)
	if err != nil {
		return nil, fmt.Errorf("legacy-history dumper failed: %w", err)
	}
	// UseNumber is load-bearing, not tidiness: st_mtime_ns values are around
	// 1.6e18, where float64's spacing is 256 ns. Decoding them as float64 moved
	// 1600000019999999523 to 1600000019999999488 — enough to flip the very
	// rounding boundary this test exists to check.
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("legacy-history dumper output is not valid JSON: %w", err)
	}
	return doc, nil
}

// legacyHistoryInt64 reads an exact integer out of the dump.
//
// With UseNumber the dumper's integers arrive as json.Number; the float64 arm
// keeps this usable for small values that were never at risk.
func legacyHistoryInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		return i, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func legacyHistoryMustInt64(t *testing.T, parent map[string]any, key string) int64 {
	t.Helper()
	v, ok := parent[key]
	if !ok {
		t.Fatalf("%s missing", key)
	}
	i, ok := legacyHistoryInt64(v)
	if !ok {
		t.Fatalf("%s is not an integer: %#v", key, v)
	}
	return i
}

// legacyHistoryPinOffset picks the offset to pin the interpreter to, refusing to
// continue if the machine's local offset is not constant across the pinned
// mtimes: a DST boundary in the middle would make the comparison meaningless
// rather than wrong.
func legacyHistoryPinOffset(doc map[string]any) (int, error) {
	cases, _ := doc["fallback_timestamp"].([]any)
	if len(cases) == 0 {
		return 0, fmt.Errorf("reference reported no fallback_timestamp cases")
	}
	pin, have := 0, false
	for _, item := range cases {
		c, _ := item.(map[string]any)
		ns, ok := legacyHistoryInt64(c["mtime_ns"])
		if !ok {
			return 0, fmt.Errorf("fallback_timestamp case has no mtime_ns")
		}
		_, off := time.Unix(0, ns).Zone()
		if !have {
			pin, have = off, true
			continue
		}
		if off != pin {
			return 0, fmt.Errorf(
				"local UTC offset changes across the pinned mtimes (%d vs %d); the "+
					"fallback-timestamp comparison would not be meaningful here", pin, off)
		}
	}
	return pin, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func legacyHistoryObj(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	obj, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s missing or not an object", key)
	}
	return obj
}

func legacyHistoryList(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	list, ok := parent[key].([]any)
	if !ok {
		t.Fatalf("%s missing or not a list", key)
	}
	return list
}

func legacyHistoryStrings(t *testing.T, parent map[string]any, key string) []string {
	t.Helper()
	list := legacyHistoryList(t, parent, key)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s contains a non-string: %#v", key, item)
		}
		out = append(out, s)
	}
	return out
}

func legacyHistoryHex(t *testing.T, parent map[string]any, key string) []byte {
	t.Helper()
	s, ok := parent[key].(string)
	if !ok {
		t.Fatalf("%s missing or not a string", key)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("%s is not hex: %v", key, err)
	}
	return b
}

func legacyHistoryOptHex(t *testing.T, parent map[string]any, key string) ([]byte, bool) {
	t.Helper()
	v, present := parent[key]
	if !present || v == nil {
		return nil, false
	}
	return legacyHistoryHex(t, parent, key), true
}

func legacyHistoryDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// legacyHistoryProbeStore builds a store whose legacy file exists but whose
// construction-time migration was a no-op, so the individual helpers can be
// driven with a pinned mtime. The file is created AFTER NewMemoryStore for
// exactly that reason.
func legacyHistoryProbeStore(t *testing.T, doc map[string]any) *memory.MemoryStore {
	t.Helper()
	ws := t.TempDir()
	store, err := memory.NewMemoryStore(ws, 0)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	path := filepath.Join(ws, "memory", "HISTORY.md")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("write legacy placeholder: %v", err)
	}
	ns := legacyHistoryMustInt64(t, doc, "mtime_ns")
	tm := time.Unix(0, ns)
	if err := os.Chtimes(path, tm, tm); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	return store
}

// ---------------------------------------------------------------------------
// _parse_legacy_history / _split_legacy_history_chunks
// ---------------------------------------------------------------------------

// TestLegacyHistoryParseMatchesPython compares both the chunk list and the
// final entries.
//
// The chunk list matters on its own: a wrong split can still produce plausible
// entries, and the split is where the [RAW]-chunk and blank-separator rules
// live. The cases also include one per whitespace code point Python's `\s`
// accepts and one per digit block where the reference's Unicode tables are
// newer than Go's, because a narrow character class changes the CHUNKING and is
// therefore observable from outside the memory package.
func TestLegacyHistoryParseMatchesPython(t *testing.T) {
	doc, _ := legacyHistoryReference(t)
	cases := legacyHistoryList(t, doc, "parse_cases")
	if len(cases) == 0 {
		t.Fatal("parse_cases is empty — a comparison over zero cases is not a pass")
	}

	store := legacyHistoryProbeStore(t, doc)

	chunksCompared := 0
	entriesCompared := 0
	normalizedCompared := 0
	for _, item := range cases {
		tc, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("parse_cases entry is not an object: %#v", item)
		}
		name, _ := tc["name"].(string)
		text, _ := tc["text"].(string)

		// The reference records the string _parse_legacy_history computed on
		// memory.py:146, captured through the real code path. Comparing it
		// directly means a normalisation bug is reported as such instead of
		// surfacing as a confusing chunk difference.
		if wantNormalized, ok := tc["normalized"].(string); ok {
			if got := memory.NormalizeLegacyHistoryText(text); got != wantNormalized {
				t.Errorf("%s: normalized = %q, want %q", name, got, wantNormalized)
			}
			normalizedCompared++
		}

		wantChunks := legacyHistoryStrings(t, tc, "chunks")
		gotChunks := store.LegacyHistoryChunks(text)
		if !equalStringSlices(gotChunks, wantChunks) {
			t.Errorf("%s: chunks = %#v, want %#v", name, gotChunks, wantChunks)
		}
		chunksCompared += len(wantChunks)

		wantEntries := legacyHistoryList(t, tc, "entries")
		gotEntries := store.ParseLegacyHistory(text)
		if len(gotEntries) != len(wantEntries) {
			t.Errorf("%s: %d entries, want %d (%#v)",
				name, len(gotEntries), len(wantEntries), gotEntries)
			continue
		}
		for i, raw := range wantEntries {
			we, _ := raw.(map[string]any)
			wantCursor := int(legacyHistoryMustInt64(t, we, "cursor"))
			wantTimestamp, _ := we["timestamp"].(string)
			wantContent, _ := we["content"].(string)

			got := gotEntries[i]
			if got.Cursor != wantCursor {
				t.Errorf("%s[%d]: cursor = %d, want %d", name, i, got.Cursor, wantCursor)
			}
			if got.Timestamp != wantTimestamp {
				t.Errorf("%s[%d]: timestamp = %q, want %q", name, i, got.Timestamp, wantTimestamp)
			}
			if got.Content != wantContent {
				t.Errorf("%s[%d]: content = %q, want %q", name, i, got.Content, wantContent)
			}
			if got.SessionKey != nil {
				t.Errorf("%s[%d]: session_key = %q, want absent", name, i, *got.SessionKey)
			}
			entriesCompared++
		}
	}
	t.Logf("parse_cases: %d cases, %d chunks and %d entries compared, %d normalisations",
		len(cases), chunksCompared, entriesCompared, normalizedCompared)
}

// TestLegacyHistoryCharClassCoverageIsComplete is a guard on the guard.
//
// The port widens Python's `\s` and `\d` by hand, and the only way to check that
// from outside the memory package is to feed the reference's code points through
// the parser. This test asserts the generated cases still cover every code point
// the dumper enumerated, so trimming PARSE_CASES later cannot silently leave the
// character classes unverified while the suite keeps passing.
func TestLegacyHistoryCharClassCoverageIsComplete(t *testing.T) {
	doc, _ := legacyHistoryReference(t)
	classes := legacyHistoryObj(t, doc, "char_classes")

	if equal, _ := classes["space_equals_str_isspace"].(bool); !equal {
		t.Error("reference says `\\s` differs from str.isspace(); the premise of the " +
			"Go character class changed")
	}

	countRangePoints := func(key string) int {
		total := 0
		for _, raw := range legacyHistoryList(t, classes, key) {
			pair, ok := raw.([]any)
			if !ok || len(pair) != 2 {
				t.Fatalf("%s entry is not a [lo, hi] pair: %#v", key, raw)
			}
			lo := legacyHistoryMustInt64(t, map[string]any{"v": pair[0]}, "v")
			hi := legacyHistoryMustInt64(t, map[string]any{"v": pair[1]}, "v")
			total += int(hi-lo) + 1
		}
		return total
	}

	spacePoints := countRangePoints("space_ranges")
	digitPoints := countRangePoints("digit_ranges")
	if spacePoints == 0 || digitPoints == 0 {
		t.Fatal("the reference reported an empty character class")
	}

	cases := legacyHistoryList(t, doc, "parse_cases")
	wsCases := 0
	digitCases := 0
	for _, item := range cases {
		tc, _ := item.(map[string]any)
		name, _ := tc["name"].(string)
		switch {
		case strings.HasPrefix(name, "ws_after_timestamp_u"):
			wsCases++
		case strings.HasPrefix(name, "unicode16_digits_u"):
			digitCases++
		}
	}
	if wsCases != spacePoints {
		t.Errorf("parse_cases cover %d of the reference's %d whitespace code points",
			wsCases, spacePoints)
	}
	// The digit class is checked through the blocks Go's Unicode tables lack;
	// every one of them must have a case.
	if digitCases != 7 {
		t.Errorf("parse_cases cover %d Unicode-16-only digit blocks, want 7", digitCases)
	}
	t.Logf("char_classes: %d whitespace code points and %d digit code points enumerated; "+
		"%d whitespace and %d digit-block cases", spacePoints, digitPoints, wsCases, digitCases)
}

// TestLegacyHistoryChunkSplitIsNotEntryBased pins the two rules that make the
// split non-obvious, independently of the dumper.
//
// A [RAW] archive checkpoint replays a transcript whose lines look exactly like
// entries, and a blank line ends a paragraph even when the next line is not an
// entry start. Both were confirmed against the reference; this test exists so a
// later "simplification" of the splitter fails loudly rather than silently
// re-chunking archived transcripts.
func TestLegacyHistoryChunkSplitIsNotEntryBased(t *testing.T) {
	doc, _ := legacyHistoryReference(t)
	store := legacyHistoryProbeStore(t, doc)

	raw := "[2024-01-01 10:00] [RAW] 2 messages\n" +
		"[t] USER: hi\n" +
		"[2024-01-02 11:00] ASSISTANT [tools: read]: done"
	if got := store.LegacyHistoryChunks(raw); len(got) != 1 {
		t.Errorf("[RAW] checkpoint was split into %d chunks: %#v", len(got), got)
	}

	// The same second line, but not inside a [RAW] chunk, must start an entry.
	notRaw := "[2024-01-01 10:00] normal\n" +
		"[2024-01-02 11:00] ASSISTANT [tools: read]: done"
	if got := store.LegacyHistoryChunks(notRaw); len(got) != 2 {
		t.Errorf("entry start was not honoured outside a [RAW] chunk: %#v", got)
	}

	// A [RAW] chunk only absorbs lines matching the raw-message pattern.
	lowercase := "[2024-01-01 10:00] [RAW] first\n[2024-01-02 11:00] user: lowercase"
	if got := store.LegacyHistoryChunks(lowercase); len(got) != 2 {
		t.Errorf("a lowercase role should not be absorbed by a [RAW] chunk: %#v", got)
	}

	blank := "paragraph one\n\nparagraph two"
	if got := store.LegacyHistoryChunks(blank); len(got) != 2 {
		t.Errorf("a blank line should end a paragraph: %#v", got)
	}
}

// ---------------------------------------------------------------------------
// _legacy_fallback_timestamp
// ---------------------------------------------------------------------------

// TestLegacyHistoryFallbackTimestampMatchesPython compares the local-time
// formatting of the legacy file's mtime.
//
// Two cases discriminate CPython's round-half-even on the sub-second part:
// st_mtime_ns 1600000019999999523 formats as the NEXT minute because CPython
// rounds the timeval to microseconds before calling localtime(), while Go's
// Time.Format truncates. The test also asserts the interpreter really applied
// the pinned offset and that the filesystem kept the exact nanosecond value, so
// a mismatch can never be blamed on the harness.
func TestLegacyHistoryFallbackTimestampMatchesPython(t *testing.T) {
	doc, pin := legacyHistoryReference(t)
	cases := legacyHistoryList(t, doc, "fallback_timestamp")
	if len(cases) == 0 {
		t.Fatal("fallback_timestamp is empty — a comparison over zero cases is not a pass")
	}

	ws := t.TempDir()
	store, err := memory.NewMemoryStore(ws, 0)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	path := filepath.Join(ws, "memory", "HISTORY.md")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	for _, item := range cases {
		tc, _ := item.(map[string]any)
		name, _ := tc["name"].(string)
		want, _ := tc["expected"].(string)
		ns := legacyHistoryMustInt64(t, tc, "mtime_ns")

		if off := legacyHistoryMustInt64(t, tc, "offset_seconds"); int(off) != pin {
			t.Fatalf("%s: reference applied UTC offset %d but the test pinned %d — "+
				"the pinning did not take effect", name, off, pin)
		}

		tm := time.Unix(0, ns)
		if err := os.Chtimes(path, tm, tm); err != nil {
			t.Fatalf("%s: Chtimes: %v", name, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: Stat: %v", name, err)
		}
		if got := fi.ModTime().UnixNano(); got != ns {
			t.Fatalf("%s: filesystem kept mtime %d, want %d — the mtimes are not comparable",
				name, got, ns)
		}

		if got := store.LegacyFallbackTimestamp(); got != want {
			t.Errorf("%s (mtime_ns=%d): LegacyFallbackTimestamp() = %q, want %q",
				name, ns, got, want)
		}
	}
	t.Logf("fallback_timestamp: %d cases compared (interpreter pinned to UTC offset %d s)",
		len(cases), pin)
}

// ---------------------------------------------------------------------------
// _next_legacy_backup_path
// ---------------------------------------------------------------------------

// TestLegacyHistoryBackupPathMatchesPython covers 0, 1 and 2 pre-existing
// backups. The numbering skips a missing name rather than counting, which is
// only visible once .bak exists but .bak.2 does not.
func TestLegacyHistoryBackupPathMatchesPython(t *testing.T) {
	doc, _ := legacyHistoryReference(t)
	cases := legacyHistoryList(t, doc, "backup_path")
	if len(cases) == 0 {
		t.Fatal("backup_path is empty — a comparison over zero cases is not a pass")
	}

	ws := t.TempDir()
	store, err := memory.NewMemoryStore(ws, 0)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	memDir := filepath.Join(ws, "memory")

	for _, item := range cases {
		tc, _ := item.(map[string]any)
		existing := legacyHistoryMustInt64(t, tc, "existing")
		want, _ := tc["chosen"].(string)

		stale, err := filepath.Glob(filepath.Join(memDir, "HISTORY.md.bak*"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		for _, p := range stale {
			if err := os.RemoveAll(p); err != nil {
				t.Fatalf("remove stale backup: %v", err)
			}
		}
		for i := 0; i < int(existing); i++ {
			name := "HISTORY.md.bak"
			if i > 0 {
				name = fmt.Sprintf("HISTORY.md.bak.%d", i+1)
			}
			if err := os.WriteFile(filepath.Join(memDir, name), []byte("old"), 0o644); err != nil {
				t.Fatalf("write backup: %v", err)
			}
		}

		if got := filepath.Base(store.NextLegacyBackupPath()); got != want {
			t.Errorf("%d existing backups: chose %q, want %q", int(existing), got, want)
		}
	}
	t.Logf("backup_path: %d cases compared", len(cases))
}

// ---------------------------------------------------------------------------
// _maybe_migrate_legacy_history
// ---------------------------------------------------------------------------

// TestLegacyHistoryMigrationMatchesPython is the end-to-end comparison.
//
// For every scenario it rebuilds the exact input the reference used, runs the
// Go constructor, and compares the whole memory/ directory listing, the bytes of
// history.jsonl, both cursor files, and the bytes of every backup. That covers
// the guards (nothing changes when the legacy file is missing or history.jsonl
// is already non-empty), the empty-legacy case (backup but no history.jsonl),
// the numbered-backup selection, and the errors="replace" decoding of invalid
// UTF-8, whose byte-level behaviour differs from strings.ToValidUTF8.
func TestLegacyHistoryMigrationMatchesPython(t *testing.T) {
	doc, _ := legacyHistoryReference(t)
	cases := legacyHistoryList(t, doc, "migration")
	if len(cases) == 0 {
		t.Fatal("migration is empty — a comparison over zero cases is not a pass")
	}

	checks := 0
	for _, item := range cases {
		tc, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("migration entry is not an object: %#v", item)
		}
		name, _ := tc["name"].(string)
		inputs := legacyHistoryObj(t, tc, "inputs")

		ws := t.TempDir()
		memDir := filepath.Join(ws, "memory")
		if err := os.MkdirAll(memDir, 0o755); err != nil {
			t.Fatalf("%s: mkdir: %v", name, err)
		}

		isDir, _ := inputs["legacy_is_dir"].(bool)
		legacyPath := filepath.Join(memDir, "HISTORY.md")
		switch legacyBytes, present := legacyHistoryOptHex(t, inputs, "legacy_bytes_hex"); {
		case isDir:
			if err := os.MkdirAll(legacyPath, 0o755); err != nil {
				t.Fatalf("%s: mkdir legacy: %v", name, err)
			}
		case present:
			if err := os.WriteFile(legacyPath, legacyBytes, 0o644); err != nil {
				t.Fatalf("%s: write legacy: %v", name, err)
			}
		}

		if v, present := inputs["legacy_mtime_ns"]; present && v != nil {
			ns, ok := legacyHistoryInt64(v)
			if !ok {
				t.Fatalf("%s: legacy_mtime_ns is not an integer: %#v", name, v)
			}
			tm := time.Unix(0, ns)
			if err := os.Chtimes(legacyPath, tm, tm); err != nil {
				t.Fatalf("%s: Chtimes: %v", name, err)
			}
		}

		if pre, present := legacyHistoryOptHex(t, inputs, "pre_history_hex"); present {
			if err := os.WriteFile(filepath.Join(memDir, "history.jsonl"), pre, 0o644); err != nil {
				t.Fatalf("%s: write history: %v", name, err)
			}
		}

		backupsHex := legacyHistoryObj(t, inputs, "backups_hex")
		for fileName, payload := range backupsHex {
			raw, err := hex.DecodeString(payload.(string))
			if err != nil {
				t.Fatalf("%s: backup %s is not hex: %v", name, fileName, err)
			}
			if err := os.WriteFile(filepath.Join(memDir, fileName), raw, 0o644); err != nil {
				t.Fatalf("%s: write backup: %v", name, err)
			}
		}

		wantBefore := legacyHistoryStrings(t, tc, "before")
		if got := legacyHistoryDirNames(t, memDir); !equalStringSlices(got, wantBefore) {
			t.Fatalf("%s: the harness built the wrong input: %#v, reference had %#v",
				name, got, wantBefore)
		}

		if _, err := memory.NewMemoryStore(ws, 0); err != nil {
			t.Fatalf("%s: NewMemoryStore: %v", name, err)
		}

		gotAfter := legacyHistoryDirNames(t, memDir)
		wantAfter := legacyHistoryStrings(t, tc, "after")
		if !equalStringSlices(gotAfter, wantAfter) {
			t.Errorf("%s: memory/ after migration = %#v, want %#v", name, gotAfter, wantAfter)
		}
		checks++

		for _, spec := range []struct{ key, file string }{
			{"history_bytes_hex", "history.jsonl"},
			{"cursor", ".cursor"},
			{"dream_cursor", ".dream_cursor"},
		} {
			want, wantPresent := legacyHistoryOptHex(t, tc, spec.key)
			got, err := os.ReadFile(filepath.Join(memDir, spec.file))
			gotPresent := err == nil
			if gotPresent != wantPresent {
				t.Errorf("%s: %s exists = %v, want %v", name, spec.file, gotPresent, wantPresent)
				continue
			}
			if !wantPresent {
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: %s = %q, want %q", name, spec.file, got, want)
			}
			checks++
		}

		wantBackups := legacyHistoryObj(t, tc, "backups_after")
		gotBackups := map[string][]byte{}
		for _, fileName := range gotAfter {
			if !strings.HasPrefix(fileName, "HISTORY.md") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(memDir, fileName))
			if err != nil {
				continue // a directory, e.g. the legacy_is_a_directory case
			}
			gotBackups[fileName] = raw
		}
		if len(gotBackups) != len(wantBackups) {
			t.Errorf("%s: %d backup files, want %d (%v vs %v)",
				name, len(gotBackups), len(wantBackups), legacyHistorySortedByteKeys(gotBackups), legacyHistorySortedAnyKeys(wantBackups))
		}
		for fileName, wantHex := range wantBackups {
			want, err := hex.DecodeString(wantHex.(string))
			if err != nil {
				t.Fatalf("%s: backups_after[%s] is not hex: %v", name, fileName, err)
			}
			if !bytes.Equal(gotBackups[fileName], want) {
				t.Errorf("%s: %s = %q, want %q", name, fileName, gotBackups[fileName], want)
			}
			checks++
		}
	}
	t.Logf("migration: %d cases compared, %d assertions", len(cases), checks)
}

func equalStringSlices(a, b []string) bool {
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

func legacyHistorySortedByteKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func legacyHistorySortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
