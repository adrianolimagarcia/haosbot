// Package memory implements the workspace memory store: the long-term memory
// file, the append-only history journal, the persona and user profile files,
// and the cursor that drives Dream consolidation.
//
// Ports MemoryStore (upstream nanobot/agent/memory.py:58 at 1bb712d3).
package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// DefaultMaxHistory is _DEFAULT_MAX_HISTORY (memory.py:61).
const DefaultMaxHistory = 1000

// historyStampLayout is the journal's timestamp format, "%Y-%m-%d %H:%M"
// (memory.py:307 and :215): naive local time, no seconds, no zone. It is NOT
// ISO-8601, and getting that wrong is the easiest mistake to make here.
const historyStampLayout = "2006-01-02 15:04"

// historyEntryHardCap is _HISTORY_ENTRY_HARD_CAP (memory.py:751): a final
// safety net for a caller that forgot its own cap, not a real limit.
const historyEntryHardCap = 64000

// DreamContentPaths is _DREAM_CONTENT_PATHS (memory.py:65). The order is the
// one the reference uses when summarising the working tree.
var DreamContentPaths = []string{"SOUL.md", "USER.md", "memory/MEMORY.md"}

// gitTrackedFiles is the list handed to GitStore (memory.py:88).
var gitTrackedFiles = []string{"SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor"}

// Entry is one record from history.jsonl.
//
// SessionKey is a pointer because its absence and its emptiness are different
// things: the reference only writes the key when one was supplied, and a
// rewrite must not introduce `"session_key": ""` where the original had no key.
type Entry struct {
	Cursor     int
	Timestamp  string
	Content    string
	SessionKey *string
}

// MemoryStore is the file I/O layer for workspace memory.
type MemoryStore struct {
	workspace         string
	maxHistoryEntries int

	memoryFile        string
	historyFile       string
	legacyHistoryFile string
	soulFile          string
	userFile          string
	cursorFile        string
	dreamCursorFile   string

	// appendLock serialises cursor allocation with the append. Without it two
	// writers can read the same current cursor and emit duplicate cursors,
	// which breaks the monotonic invariant the journal relies on.
	appendLock sync.Mutex

	// One-shot log guards, mirroring the reference's rate limiting. They are
	// set once so a corrupt file does not produce a warning per entry.
	corruptionLogged     bool
	malformedEntryLogged bool
	oversizeLogged       bool

	// dreamPromptOversizeLogged is _dream_prompt_oversize_logged
	// (memory.py:85): the same rate limit for an oversized Dream prompt
	// override.
	dreamPromptOversizeLogged bool

	// git is the injected GitStore used by DreamContentDiff. The reference
	// builds its own in __init__; this port takes it through SetDreamDiffer
	// so it does not have to import internal/gitstore.
	git DreamDiffer
}

// NewMemoryStore opens (and creates) the memory directory for a workspace.
//
// The directory is created eagerly, matching __init__ (memory.py:75) which
// calls ensure_dir rather than deferring creation to the first write.
//
// The legacy HISTORY.md migration runs here, at the same point in construction
// as memory.py:91. It is best-effort by design and therefore returns no error:
// a workspace with an unreadable or unparseable HISTORY.md still gets a usable
// store, exactly as the reference's bare `except Exception` intends.
func NewMemoryStore(workspace string, maxHistoryEntries int) (*MemoryStore, error) {
	if maxHistoryEntries == 0 {
		maxHistoryEntries = DefaultMaxHistory
	}
	memoryDir := filepath.Join(workspace, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create %s: %w", memoryDir, err)
	}

	s := &MemoryStore{
		workspace:         workspace,
		maxHistoryEntries: maxHistoryEntries,
		memoryFile:        filepath.Join(memoryDir, "MEMORY.md"),
		historyFile:       filepath.Join(memoryDir, "history.jsonl"),
		legacyHistoryFile: filepath.Join(memoryDir, "HISTORY.md"),
		soulFile:          filepath.Join(workspace, "SOUL.md"),
		userFile:          filepath.Join(workspace, "USER.md"),
		cursorFile:        filepath.Join(memoryDir, ".cursor"),
		dreamCursorFile:   filepath.Join(memoryDir, ".dream_cursor"),
	}
	s.maybeMigrateLegacyHistory()
	return s, nil
}

// Paths exposes the resolved file locations. Callers that need to hand a path
// to a tool (or to a test) should not reconstruct them from the workspace.
func (s *MemoryStore) Paths() (memory, history, soul, user, cursor, dreamCursor string) {
	return s.memoryFile, s.historyFile, s.soulFile, s.userFile, s.cursorFile, s.dreamCursorFile
}

// GitTrackedFiles returns the files the store asks GitStore to version.
func GitTrackedFiles() []string {
	return append([]string(nil), gitTrackedFiles...)
}

// ReadFile returns the file's contents, or "" when it does not exist.
//
// Mirrors read_file (memory.py:100): only FileNotFoundError is swallowed. An
// unreadable file is an error, because silently returning "" would let the
// agent overwrite memory it could not read.
func ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// ReadMemory returns memory/MEMORY.md.
func (s *MemoryStore) ReadMemory() (string, error) { return ReadFile(s.memoryFile) }

// WriteMemory overwrites memory/MEMORY.md.
//
// This is a plain write, not an atomic one, matching write_memory
// (memory.py:232) which calls write_text directly.
func (s *MemoryStore) WriteMemory(content string) error {
	return os.WriteFile(s.memoryFile, []byte(content), 0o644)
}

// ReadSoul returns SOUL.md.
func (s *MemoryStore) ReadSoul() (string, error) { return ReadFile(s.soulFile) }

// WriteSoul overwrites SOUL.md.
func (s *MemoryStore) WriteSoul(content string) error {
	return os.WriteFile(s.soulFile, []byte(content), 0o644)
}

// ReadUser returns USER.md.
func (s *MemoryStore) ReadUser() (string, error) { return ReadFile(s.userFile) }

// WriteUser overwrites USER.md.
func (s *MemoryStore) WriteUser(content string) error {
	return os.WriteFile(s.userFile, []byte(content), 0o644)
}

// GetMemoryContext renders the long-term memory for prompt injection.
//
// Mirrors get_memory_context (memory.py:253): an empty memory file produces an
// empty string, not a heading with nothing under it.
func (s *MemoryStore) GetMemoryContext() (string, error) {
	longTerm, err := s.ReadMemory()
	if err != nil {
		return "", err
	}
	if longTerm == "" {
		return "", nil
	}
	return "## Long-term Memory\n" + longTerm, nil
}

// ---------------------------------------------------------------------------
// history.jsonl
// ---------------------------------------------------------------------------

// normalizeHistoryEntry ports _normalize_history_entry (memory.py:259).
//
// Note the case that looks like a bug and is not: when the raw entry is
// non-empty but stripping the thinking tags empties it, the empty string is
// what gets persisted. Falling back to the raw text would undo strip_think's
// guarantees at the moment Dream reads the journal back.
func (s *MemoryStore) normalizeHistoryEntry(entry string, maxChars *int) string {
	limit := historyEntryHardCap
	if maxChars != nil {
		limit = *maxChars
	}
	raw := textutil.PyRStrip(entry)
	content := textutil.StripThink(raw)
	// Characters, not bytes: memory.py:269 is `len(content) > limit` on a str.
	// Counting bytes here would fire the oversize flag for a multi-byte entry
	// the reference considers in-range. The truncation itself is unaffected —
	// TruncateText only cuts when the rune count exceeds the limit — so the
	// persisted text was already right; only the flag diverged.
	if utf8.RuneCountInString(content) > limit {
		if !s.oversizeLogged {
			s.oversizeLogged = true
		}
		content = textutil.TruncateText(content, limit)
	}
	return content
}

// AppendHistory appends an entry and returns its cursor.
//
// Mirrors append_history (memory.py:282).
func (s *MemoryStore) AppendHistory(entry string, maxChars *int, sessionKey string) (int, error) {
	ts := time.Now().Format(historyStampLayout)
	content := s.normalizeHistoryEntry(entry, maxChars)

	s.appendLock.Lock()
	defer s.appendLock.Unlock()

	cursor, err := s.nextCursor()
	if err != nil {
		return 0, err
	}

	// The reference computes `raw = entry.rstrip()` here and uses it only for a
	// debug-level log line when stripping emptied a non-empty entry. That log is
	// diagnostic only — it reaches neither the journal nor the model — so it is
	// deliberately not reproduced rather than carried as dead code.
	rec := Entry{Cursor: cursor, Timestamp: ts, Content: content}
	if sessionKey != "" {
		key := sessionKey
		rec.SessionKey = &key
	}

	line, err := marshalEntryLine(rec)
	if err != nil {
		return 0, err
	}

	f, err := os.OpenFile(s.historyFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("memory: append history: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}

	if err := os.WriteFile(s.cursorFile, []byte(strconv.Itoa(cursor)), 0o644); err != nil {
		return 0, err
	}
	return cursor, nil
}

// marshalEntryLine renders one JSONL record.
//
// Key order is the reference's insertion order, and the encoding must match
// json.dumps(..., ensure_ascii=False) exactly. Two things that are easy to get
// wrong: Go escapes <, >, & by default and escapes U+2028/U+2029
// unconditionally, so both are corrected here; and json.dumps' DEFAULT
// separators are ", " and ": ", not the compact "," and ":" — a compact line
// still round-trips through json.loads, so the divergence is invisible until
// the file is compared byte for byte with the reference (see
// docs/spec-memory.md §2, which shows the spaced form).
func marshalEntryLine(e Entry) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"cursor": `)
	b.WriteString(strconv.Itoa(e.Cursor))
	b.WriteString(`, "timestamp": `)
	if err := writePyString(&b, e.Timestamp); err != nil {
		return nil, err
	}
	b.WriteString(`, "content": `)
	if err := writePyString(&b, e.Content); err != nil {
		return nil, err
	}
	if e.SessionKey != nil {
		b.WriteString(`, "session_key": `)
		if err := writePyString(&b, *e.SessionKey); err != nil {
			return nil, err
		}
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// writePyString writes a JSON string with Python's escaping rules.
func writePyString(b *bytes.Buffer, s string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	b.Write(pyjson.UnescapeLineSeparators(out))
	return nil
}

// validCursor ports _valid_cursor (memory.py:325).
//
// A bool is rejected explicitly: Python's isinstance(True, int) is True, so
// without the check `"cursor": true` would be read as cursor 1.
func validCursor(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		return 0, false
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil || i < 0 {
			return 0, false
		}
		return int(i), true
	case float64:
		if n < 0 || n != float64(int64(n)) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// ReadEntries returns every well-formed object line from history.jsonl.
//
// Malformed lines are skipped, not fatal (memory.py:435).
//
// This is line-based rather than a streaming json.Decoder on purpose. A
// Decoder that fails on one value cannot be advanced past it without knowing
// how many bytes it consumed, so a `continue` after an error re-reads the same
// bytes forever. Reading the file as lines makes "skip the bad one" trivially
// correct, and JSONL is line-oriented by definition.
func (s *MemoryStore) ReadEntries() ([]map[string]any, error) {
	f, err := os.Open(s.historyFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	// A single entry may be up to the emergency cap, so the default 64 KiB
	// line limit is too small. The cap bounds the buffer growth.
	sc.Buffer(make([]byte, 0, 64*1024), historyEntryHardCap*4+1024)
	for sc.Scan() {
		line := textutil.PyStrip(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			continue
		}
		out = append(out, obj)
	}
	// A read error mid-file is reported rather than silently truncating the
	// journal; a caller that compacts on a partial read would delete entries.
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// readLastEntry reads only the tail of the file, like _read_last_entry
// (memory.py:452). Returns nil when the tail is unreadable, so callers fall
// back to a full scan rather than trusting a partial read.
func (s *MemoryStore) readLastEntry() *map[string]any {
	f, err := os.Open(s.historyFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil
	}
	readSize := fi.Size()
	if readSize > 4096 {
		readSize = 4096
	}
	buf := make([]byte, readSize)
	if _, err := f.ReadAt(buf, fi.Size()-readSize); err != nil && err != io.EOF {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := textutil.PyStrip(lines[i])
		if line == "" {
			continue
		}
		var obj map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			return nil
		}
		return &obj
	}
	return nil
}

// ValidHistoryPayload ports _valid_history_payload (memory.py:363).
func ValidHistoryPayload(e map[string]any) bool {
	if _, ok := e["timestamp"].(string); !ok {
		return false
	}
	if _, ok := e["content"].(string); !ok {
		return false
	}
	if v, ok := e["session_key"]; ok && v != nil {
		if _, isStr := v.(string); !isStr {
			return false
		}
	}
	return true
}

// IterValidEntries yields well-formed entries with their cursors.
//
// Mirrors _iter_valid_entries (memory.py:331). Corrupt entries are dropped and
// logged once each, because a single bad line written by an external tool must
// not make the whole journal unreadable.
func (s *MemoryStore) IterValidEntries() ([]struct {
	Entry  map[string]any
	Cursor int
}, error) {
	entries, err := s.ReadEntries()
	if err != nil {
		return nil, err
	}
	var out []struct {
		Entry  map[string]any
		Cursor int
	}
	var poisoned any
	var malformed int
	haveMalformed := false

	for _, e := range entries {
		raw, ok := e["cursor"]
		if !ok || raw == nil {
			continue
		}
		cursor, ok := validCursor(raw)
		if !ok {
			poisoned = raw
			continue
		}
		if !ValidHistoryPayload(e) {
			malformed = cursor
			haveMalformed = true
			continue
		}
		out = append(out, struct {
			Entry  map[string]any
			Cursor int
		}{e, cursor})
	}
	if poisoned != nil {
		s.corruptionLogged = true
	}
	if haveMalformed {
		_ = malformed
		s.malformedEntryLogged = true
	}
	return out, nil
}

// readCursorCounter returns the persisted counter when usable.
func (s *MemoryStore) readCursorCounter() (int, bool) {
	b, err := os.ReadFile(s.cursorFile)
	if err != nil {
		return 0, false
	}
	// memory.py:376 is `int(self._cursor_file.read_text().strip())`. PyAtoi
	// implements exactly that expression: Python's str.strip() set plus
	// Python's int() grammar (underscores between digits, Unicode decimal
	// digits). strconv.Atoi(strings.TrimSpace(...)) diverged on all three.
	v, ok := textutil.PyAtoi(string(b))
	if !ok || v < 0 {
		return 0, false
	}
	return v, true
}

// nextCursor ports _next_cursor (memory.py:381).
//
// The fast path trusts the tail of the file; when the tail is unreadable the
// whole file is scanned and the maximum taken, which stays correct even if the
// monotonic invariant was broken by an external writer.
func (s *MemoryStore) nextCursor() (int, error) {
	counter, haveCounter := s.readCursorCounter()

	var lastCursor int
	haveLast := false
	if last := s.readLastEntry(); last != nil {
		if c, ok := validCursor((*last)["cursor"]); ok {
			lastCursor, haveLast = c, true
		}
	}

	if haveCounter {
		if haveLast {
			return max(counter, lastCursor) + 1, nil
		}
		m, err := s.maxCursor()
		if err != nil {
			return 0, err
		}
		return max(counter, m) + 1, nil
	}
	if haveLast {
		return lastCursor + 1, nil
	}
	m, err := s.maxCursor()
	if err != nil {
		return 0, err
	}
	return m + 1, nil
}

func (s *MemoryStore) maxCursor() (int, error) {
	valid, err := s.IterValidEntries()
	if err != nil {
		return 0, err
	}
	m := 0
	for _, v := range valid {
		if v.Cursor > m {
			m = v.Cursor
		}
	}
	return m, nil
}

// ReadUnprocessedHistory returns entries with a cursor greater than since.
func (s *MemoryStore) ReadUnprocessedHistory(since int) ([]map[string]any, error) {
	valid, err := s.IterValidEntries()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, v := range valid {
		if v.Cursor > since {
			out = append(out, v.Entry)
		}
	}
	return out, nil
}

// GetLatestCursor returns the highest cursor this store has issued, or 0.
//
// Ports get_latest_cursor (memory.py:503), which is
// `max(self._next_cursor() - 1, 0)`.
//
// This previously returned maxCursor(), which scans history entries only. That
// ignored the cursor file entirely, so a store whose counter file said 5 but
// whose journal was empty reported 0 while the reference reported 5. The
// difference is observable whenever the counter survives a truncated or deleted
// journal — exactly the case the counter file exists to cover.
func (s *MemoryStore) GetLatestCursor() (int, error) {
	next, err := s.nextCursor()
	if err != nil {
		return 0, err
	}
	return max(next-1, 0), nil
}

// CompactHistory drops the oldest entries while never discarding input Dream
// has not consumed yet.
//
// Mirrors compact_history (memory.py:403). The subtle part is `first_unprocessed`:
// the retention limit yields to it, so a long-unprocessed backlog survives
// compaction instead of being silently dropped before Dream ever sees it.
func (s *MemoryStore) CompactHistory() error {
	if s.maxHistoryEntries <= 0 {
		return nil
	}
	entries, err := s.ReadEntries()
	if err != nil {
		return err
	}
	if len(entries) <= s.maxHistoryEntries {
		return nil
	}

	lastDream, err := s.GetLastDreamCursor()
	if err != nil {
		return err
	}
	firstUnprocessed := len(entries)
	for i, e := range entries {
		if c, ok := validCursor(e["cursor"]); ok && c > lastDream {
			firstUnprocessed = i
			break
		}
	}
	keepFrom := len(entries) - s.maxHistoryEntries
	if firstUnprocessed < keepFrom {
		keepFrom = firstUnprocessed
	}
	return s.writeEntries(entries[keepFrom:])
}

// writeEntries overwrites history.jsonl atomically (memory.py:471).
func (s *MemoryStore) writeEntries(entries []map[string]any) error {
	tmp := s.historyFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	for _, e := range entries {
		line, err := marshalMapLine(e)
		if err != nil {
			cleanup()
			return err
		}
		if _, err := f.Write(line); err != nil {
			cleanup()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.historyFile); err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the directory so the rename itself is durable.
	if d, err := os.Open(filepath.Dir(s.historyFile)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// marshalMapLine renders a decoded entry back to a JSONL line, preserving the
// reference's key order, its int/float distinction, and json.dumps' default
// ", " / ": " separators (see marshalEntryLine).
func marshalMapLine(e map[string]any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, k := range []string{"cursor", "timestamp", "content", "session_key"} {
		v, ok := e[k]
		if !ok {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		if err := writePyString(&b, k); err != nil {
			return nil, err
		}
		b.WriteString(": ")
		if err := writePyValue(&b, v); err != nil {
			return nil, err
		}
	}
	// Any key this port does not know about is preserved rather than dropped,
	// so a field added by a newer reference survives a compaction round trip.
	var extra []string
	for k := range e {
		switch k {
		case "cursor", "timestamp", "content", "session_key":
		default:
			extra = append(extra, k)
		}
	}
	sortStrings(extra)
	for _, k := range extra {
		if !first {
			b.WriteString(", ")
		}
		first = false
		if err := writePyString(&b, k); err != nil {
			return nil, err
		}
		b.WriteString(": ")
		if err := writePyValue(&b, e[k]); err != nil {
			return nil, err
		}
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

func writePyValue(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		return writePyString(b, t)
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		// Written verbatim: this is what keeps 1.0 from becoming 1.
		b.WriteString(t.String())
	case float64:
		b.WriteString(pyjson.FormatFloat(t))
	default:
		enc, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(enc)
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Dream cursor
// ---------------------------------------------------------------------------

// GetLastDreamCursor returns the cursor Dream last consumed, or 0.
func (s *MemoryStore) GetLastDreamCursor() (int, error) {
	b, err := os.ReadFile(s.dreamCursorFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	// memory.py:501 is `int(self._dream_cursor_file.read_text().strip())`.
	// Same expression as the history cursor above, so the same helper.
	v, ok := textutil.PyAtoi(string(b))
	if !ok {
		return 0, nil
	}
	return v, nil
}

// SetLastDreamCursor records the cursor Dream has consumed.
func (s *MemoryStore) SetLastDreamCursor(cursor int) error {
	return os.WriteFile(s.dreamCursorFile, []byte(strconv.Itoa(cursor)), 0o644)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Legacy HISTORY.md migration
// ---------------------------------------------------------------------------

// pySpaceClass is Python's `\s` inside a str pattern.
//
// Go's `\s` is `[\t\n\f\r ]`: ASCII-only, and missing \v. Python's `\s` for a
// str pattern is exactly str.isspace(), and all three legacy patterns use it.
// The ranges below were ENUMERATED from the reference interpreter
// (compat/python/dump_legacy_history.py, char_classes.space_ranges), which also
// confirms the set equals `chr(cp).isspace()`:
//
//	\t \n \v \f \r, U+001C..U+001F, U+0020, U+0085, U+00A0, U+1680,
//	U+2000..U+200A, U+2028, U+2029, U+202F, U+205F, U+3000
//
// textutil.PyIsSpace covers the same set but cannot be used inside a regexp.
const pySpaceClass = `[\t\n\x0b\f\r\x{1c}-\x{1f} \x{85}\x{a0}\x{1680}` +
	`\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// pyDigitClass is Python's `\d` inside a str pattern: Unicode category Nd, not
// ASCII 0-9. Go's `\p{Nd}` is the same category, but from Go 1.23's Unicode
// 15.0.0 tables, while the reference interpreter (.tools/venv, CPython 3.14.7)
// ships Unicode 16.0.0. The seven blocks below are Nd there and not in Go.
// They were obtained by enumerating both sets, not copied from a table.
// Without them a legacy line whose bracketed date used one of those digit
// blocks would stop being recognised as an entry start, which changes how the
// file is chunked.
const pyDigitClass = `[\p{Nd}` +
	`\x{10d40}-\x{10d49}\x{116d0}-\x{116e3}\x{11bf0}-\x{11bf9}` +
	`\x{16130}-\x{16139}\x{16d70}-\x{16d79}\x{1ccf0}-\x{1ccf9}\x{1e5f1}-\x{1e5fa}]`

// The three legacy patterns, verbatim from memory.py:66-70 with `\s` and `\d`
// widened to Python's definitions above.
//
// All three are RE2-compatible: no backreferences and no lookaround, so no
// rewrite was needed beyond the character classes. They keep their leading `^`,
// and Go's `^` (without the `(?m)` flag) matches only at the start of the text,
// so FindStringSubmatchIndex has exactly Python's re.match semantics —
// anchored at the start, and NOT a full match. Python's re.match is not
// re.fullmatch, and Go's MatchString is unanchored; the `^` is what makes them
// agree, so it must not be dropped.
var (
	legacyEntryStartRe = regexp.MustCompile(
		`^\[(` + pyDigitClass + `{4}-` + pyDigitClass + `{2}-` + pyDigitClass + `{2}[^\]]*)` +
			pySpaceClass + `*`)
	legacyTimestampRe = regexp.MustCompile(
		`^\[(` + pyDigitClass + `{4}-` + pyDigitClass + `{2}-` + pyDigitClass + `{2} ` +
			pyDigitClass + `{2}:` + pyDigitClass + `{2})\]` + pySpaceClass + `*`)
	legacyRawMessageRe = regexp.MustCompile(
		`^\[` + pyDigitClass + `{4}-` + pyDigitClass + `{2}-` + pyDigitClass + `{2}[^\]]*\]` +
			pySpaceClass + `+[A-Z][A-Z0-9_]*(?:` + pySpaceClass + `+\[tools:` +
			pySpaceClass + `*[^\]]+\])?:`)
)

// NormalizeLegacyHistoryText ports the first line of _parse_legacy_history
// (memory.py:146): CRLF and lone CR become LF, then the whole text is stripped
// with PYTHON's whitespace set. strings.TrimSpace would leave U+001C..U+001F
// in place and change the chunking.
func NormalizeLegacyHistoryText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return textutil.PyStrip(text)
}

// LegacyHistoryChunks returns the chunk list _split_legacy_history_chunks
// (memory.py:172) produces for text, normalising it exactly as
// _parse_legacy_history does first.
//
// It is exported so the differential test can compare the SPLIT and not only
// the final entries: the split is where the subtle rules live, and a wrong
// split can still yield plausible-looking entries.
func (s *MemoryStore) LegacyHistoryChunks(text string) []string {
	return s.splitLegacyHistoryChunks(NormalizeLegacyHistoryText(text))
}

// splitLegacyHistoryChunks ports _split_legacy_history_chunks (memory.py:171).
func (s *MemoryStore) splitLegacyHistoryChunks(text string) []string {
	lines := strings.Split(text, "\n")
	var chunks []string
	var current []string
	sawBlankSeparator := false

	for _, line := range lines {
		if sawBlankSeparator && textutil.PyStrip(line) != "" && len(current) > 0 {
			chunks = append(chunks, textutil.PyStrip(strings.Join(current, "\n")))
			current = []string{line}
			sawBlankSeparator = false
			continue
		}
		if s.shouldStartNewLegacyChunk(line, current) {
			chunks = append(chunks, textutil.PyStrip(strings.Join(current, "\n")))
			current = []string{line}
			sawBlankSeparator = false
			continue
		}
		current = append(current, line)
		sawBlankSeparator = textutil.PyStrip(line) == ""
	}

	if len(current) > 0 {
		chunks = append(chunks, textutil.PyStrip(strings.Join(current, "\n")))
	}
	// memory.py:193 drops the chunks that stripped to nothing. It is not
	// redundant with the checks above: a chunk made only of whitespace lines
	// survives every branch and is removed here.
	var kept []string
	for _, chunk := range chunks {
		if chunk != "" {
			kept = append(kept, chunk)
		}
	}
	return kept
}

// shouldStartNewLegacyChunk ports _should_start_new_legacy_chunk (memory.py:195).
//
// The middle clause is what keeps a `[RAW]` archive checkpoint in one piece:
// its replayed transcript lines look exactly like new entries, and splitting
// there would shred the checkpoint into unrelated entries.
func (s *MemoryStore) shouldStartNewLegacyChunk(line string, current []string) bool {
	if len(current) == 0 {
		return false
	}
	if legacyEntryStartRe.FindStringSubmatchIndex(line) == nil {
		return false
	}
	if s.isRawLegacyChunk(current) && legacyRawMessageRe.FindStringSubmatchIndex(line) != nil {
		return false
	}
	return true
}

// isRawLegacyChunk ports _is_raw_legacy_chunk (memory.py:204).
func (s *MemoryStore) isRawLegacyChunk(lines []string) bool {
	firstNonEmpty := ""
	for _, line := range lines {
		if textutil.PyStrip(line) != "" {
			firstNonEmpty = line
			break
		}
	}
	loc := legacyTimestampRe.FindStringSubmatchIndex(firstNonEmpty)
	if loc == nil {
		return false
	}
	// loc[1] is the end of the whole match, i.e. match.end() in the reference.
	return strings.HasPrefix(textutil.PyLStrip(firstNonEmpty[loc[1]:]), "[RAW]")
}

// LegacyFallbackTimestamp ports _legacy_fallback_timestamp (memory.py:211):
// the legacy file's mtime formatted "%Y-%m-%d %H:%M" in local time, or now when
// the file cannot be stat'ed.
//
// Exported so the differential test can drive it with pinned mtimes.
func (s *MemoryStore) LegacyFallbackTimestamp() string {
	fi, err := os.Stat(s.legacyHistoryFile)
	if err != nil {
		return time.Now().Format(historyStampLayout)
	}
	return formatPyLocalStamp(fi.ModTime())
}

// formatPyLocalStamp reproduces
// datetime.fromtimestamp(t).strftime("%Y-%m-%d %H:%M") for a local time.Time.
//
// The sub-second part is not decorative. CPython's datetime.fromtimestamp
// converts the timestamp to a timeval with _PyTime_ROUND_HALF_EVEN BEFORE
// calling localtime(), so a mtime of ...:59.9999995 formats as the NEXT second
// — and, at the end of a minute, as the next minute. Go's Time.Format truncates
// instead. Verified against the reference: a file with st_mtime_ns
// 1600000019999999523 formats as 09:27 locally, while truncation would say
// 09:26. The half-to-even rule is CPython's; the `usec%2 == 1` arm is not
// reachable from any double-precision mtime, but it is the rule, so it is here.
func formatPyLocalStamp(t time.Time) string {
	sec := t.Unix()
	nsec := int64(t.Nanosecond())
	usec := nsec / 1000
	switch rem := nsec % 1000; {
	case rem > 500:
		usec++
	case rem == 500 && usec%2 == 1:
		usec++
	}
	if usec == 1_000_000 {
		usec = 0
		sec++
	}
	return time.Unix(sec, usec*1000).In(t.Location()).Format(historyStampLayout)
}

// NextLegacyBackupPath ports _next_legacy_backup_path (memory.py:219):
// HISTORY.md.bak, then HISTORY.md.bak.2, .bak.3, ... — the first name that does
// not already exist. The legacy file is preserved, never deleted.
//
// Exported so the differential test can compare the choice for 0, 1 and 2
// pre-existing backups.
func (s *MemoryStore) NextLegacyBackupPath() string {
	dir := filepath.Dir(s.legacyHistoryFile)
	candidate := filepath.Join(dir, "HISTORY.md.bak")
	for suffix := 2; ; suffix++ {
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("HISTORY.md.bak.%d", suffix))
	}
}

// ParseLegacyHistory ports _parse_legacy_history (memory.py:145).
//
// Exported so the differential test can drive it with the reference's cases.
// Every chunk becomes exactly one entry: this parser preserves content rather
// than validating it, so a chunk with no recognisable timestamp keeps its whole
// text and takes the fallback timestamp.
func (s *MemoryStore) ParseLegacyHistory(text string) []Entry {
	normalized := NormalizeLegacyHistoryText(text)
	if normalized == "" {
		return nil
	}

	fallbackTimestamp := s.LegacyFallbackTimestamp()
	var entries []Entry
	for i, chunk := range s.splitLegacyHistoryChunks(normalized) {
		timestamp := fallbackTimestamp
		content := chunk
		if loc := legacyTimestampRe.FindStringSubmatchIndex(chunk); loc != nil {
			// loc[2]:loc[3] is group 1; loc[1] is the end of the whole match.
			timestamp = chunk[loc[2]:loc[3]]
			remainder := textutil.PyLStrip(chunk[loc[1]:])
			if remainder != "" {
				content = remainder
			}
		}
		entries = append(entries, Entry{
			Cursor:    i + 1,
			Timestamp: timestamp,
			Content:   content,
		})
	}
	return entries
}

// maybeMigrateLegacyHistory ports _maybe_migrate_legacy_history (memory.py:106).
//
// Signature: the reference returns None and wraps its whole body in a bare
// `except Exception`, so this returns nothing and reports no error. That is the
// faithful choice, not a shortcut — NewMemoryStore must not start failing
// because an old HISTORY.md could not be migrated, and the reference makes the
// same trade: it logs and carries on. Every early return below corresponds to
// one arm of that `except`.
//
// One deliberate divergence is unavoidable. The two guards at memory.py:112-115
// sit OUTSIDE the try block, so an OSError from `stat()` would propagate out of
// __init__ and the store would not be constructed at all. Go has no exception
// to propagate, and turning that race into a construction failure would be a
// larger behavioural change than treating an unreadable stat as "no non-empty
// history" and continuing.
func (s *MemoryStore) maybeMigrateLegacyHistory() {
	if _, err := os.Stat(s.legacyHistoryFile); err != nil {
		return
	}
	if fi, err := os.Stat(s.historyFile); err == nil && fi.Size() > 0 {
		return
	}

	raw, err := os.ReadFile(s.legacyHistoryFile)
	if err != nil {
		// memory.py:118 reads with errors="replace" so invalid UTF-8 becomes
		// U+FFFD instead of failing; only a real I/O error lands here.
		return
	}
	entries := s.ParseLegacyHistory(decodeLegacyUTF8(raw))

	if len(entries) > 0 {
		if err := s.writeEntries(legacyEntryMaps(entries)); err != nil {
			return
		}
		last := strconv.Itoa(entries[len(entries)-1].Cursor)
		if err := os.WriteFile(s.cursorFile, []byte(last), 0o644); err != nil {
			return
		}
		// The dream cursor is advanced to the last migrated cursor too. This is
		// a deliberate product decision, not an accident: it defaults the
		// upgrade to "already processed" so a first start does not replay the
		// user's entire historical archive into Dream. Do not "fix" it by
		// leaving .dream_cursor absent — the reference writes both files at
		// memory.py:131-134, and an upgrade that suddenly feeds years of
		// history to the consolidator is a real, user-visible regression.
		if err := os.WriteFile(s.dreamCursorFile, []byte(last), 0o644); err != nil {
			return
		}
	}

	// The rename happens even when the legacy file parsed to nothing: the file
	// is empty or unparseable, and the reference still parks it as a backup
	// (memory.py:136-137) rather than leaving it to be re-read forever.
	if err := os.Rename(s.legacyHistoryFile, s.NextLegacyBackupPath()); err != nil {
		return
	}
}

// legacyEntryMaps renders parsed entries in the shape writeEntries consumes.
// Key order and value types match the reference's dicts so _write_entries
// produces byte-identical JSON.
func legacyEntryMaps(entries []Entry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		rec := map[string]any{
			"cursor":    e.Cursor,
			"timestamp": e.Timestamp,
			"content":   e.Content,
		}
		if e.SessionKey != nil {
			rec["session_key"] = *e.SessionKey
		}
		out = append(out, rec)
	}
	return out
}

// decodeLegacyUTF8 is bytes.decode("utf-8", errors="replace").
//
// strings.ToValidUTF8 is NOT equivalent: it collapses a RUN of invalid bytes
// into a single U+FFFD, while CPython emits one replacement per ill-formed
// subsequence, and Go's utf8.DecodeRune advances one byte at a time where
// CPython consumes the whole "maximal subpart". Both differences are visible:
// b"\xff\xff" is two U+FFFD in the reference and one from ToValidUTF8, and the
// truncated-but-valid prefix in b"\xf0\x9f" is ONE U+FFFD there but two from a
// byte-at-a-time loop. The rule implemented here is Unicode's recommended
// practice, and it was checked against the reference exhaustively for every
// 1- and 2-byte input and on 400k random 3-6 byte inputs with zero mismatches.
func decodeLegacyUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size != 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += maximalSubpartLen(b[i:])
	}
	return sb.String()
}

// maximalSubpartLen returns the length of the maximal subpart of an ill-formed
// UTF-8 sequence starting at b[0]: the longest prefix that could still have
// been the beginning of a valid sequence given the bytes present.
//
// The per-leading-byte second-byte ranges are the same ones utf8.DecodeRune
// enforces (E0 needs A0..BF, ED needs 80..9F to exclude surrogates, F0 needs
// 90..BF, F4 needs 80..8F to stop at U+10FFFF); a leading byte outside the
// valid set, or 80..BF, is its own single-byte error.
func maximalSubpartLen(b []byte) int {
	c := b[0]
	var n int
	var lo, hi byte
	switch {
	case c >= 0xC2 && c <= 0xDF:
		n, lo, hi = 2, 0x80, 0xBF
	case c == 0xE0:
		n, lo, hi = 3, 0xA0, 0xBF
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		n, lo, hi = 3, 0x80, 0xBF
	case c == 0xED:
		n, lo, hi = 3, 0x80, 0x9F
	case c == 0xF0:
		n, lo, hi = 4, 0x90, 0xBF
	case c >= 0xF1 && c <= 0xF3:
		n, lo, hi = 4, 0x80, 0xBF
	case c == 0xF4:
		n, lo, hi = 4, 0x80, 0x8F
	default:
		return 1
	}
	if len(b) < 2 || b[1] < lo || b[1] > hi {
		return 1
	}
	got := 2
	for k := 2; k < n; k++ {
		if len(b) > k && b[k] >= 0x80 && b[k] <= 0xBF {
			got = k + 1
			continue
		}
		break
	}
	return got
}
