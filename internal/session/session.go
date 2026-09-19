package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// Session is one conversation transcript backed by a JSONL file.
//
// It is the concrete type behind the agent loop's Transcript interface
// (internal/agent/loop.go:43).
//
// Fidelity model: every record read from disk is kept alongside the parsed
// core.Message it produced. When Save runs, a message that is still equal to
// the value parsed at load time is written back byte for byte, so fields that
// core.Message does not model — an unknown "_type", a provider-shaped
// tool_calls array, unusual key order or escaping — survive a load/save cycle
// untouched. Messages that Go created or modified are re-encoded with Python's
// escaping, separators and number formatting.
type Session struct {
	store *Store
	key   string

	mu        sync.Mutex
	messages  []core.Message
	snapshots []core.Message // value parsed at load, aligned with messages
	rawLines  [][]byte       // original record line, aligned with messages

	meta      map[string]any
	metaOrder []string
	metaExtra []rawField // unknown members of the metadata record, in order

	// metaRaw holds the exact JSON text of each metadata value as it was loaded
	// from disk or written by this package, and metaLoaded holds an independent
	// decode of those same bytes. Save reuses metaRaw for any value that is
	// still DeepEqual to metaLoaded, which preserves nested key order and number
	// formatting that a re-encode through map[string]any would lose: Python
	// dicts keep insertion order, Go maps do not.
	metaRaw    map[string]json.RawMessage
	metaLoaded map[string]any

	createdAt  time.Time
	createdStr string
	updatedAt  time.Time
	updatedStr string

	lastArchived int

	providerState json.RawMessage // validated state value, nil when absent
	providerLine  []byte          // original provider_state record line
	providerSnap  json.RawMessage // state value at load, for verbatim re-emit
}

// newSession returns an empty session for key.
func (s *Store) newSession(key string) *Session {
	now := nowTimestamp()
	return &Session{
		store:      s,
		key:        key,
		meta:       map[string]any{},
		metaRaw:    map[string]json.RawMessage{},
		metaLoaded: map[string]any{},
		createdAt:  now,
		createdStr: formatNaive(now),
		updatedAt:  now,
		updatedStr: formatNaive(now),
	}
}

// Key returns the session key.
func (s *Session) Key() string { return s.key }

// Messages returns a snapshot of the transcript.
//
// The returned slice is a copy: appending to it does not change the session.
// Use AddMessage (or SetMessage) to modify the transcript.
func (s *Session) Messages() []core.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// AddMessage appends a message to the transcript.
//
// Mirrors Session.add_message (manager.py:311-320) with one difference: the
// reference always overwrites "timestamp" with datetime.now().isoformat(),
// whereas this port only fills it in when the message does not already carry
// one, so a caller that stamps its own timestamp is not overruled. Like the
// reference, the session's updated_at is advanced.
func (s *Session) AddMessage(m core.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Timestamp == "" {
		m.Timestamp = formatNaive(nowTimestamp())
	}
	s.messages = append(s.messages, m)
	s.snapshots = append(s.snapshots, core.Message{})
	s.rawLines = append(s.rawLines, nil)
	now := nowTimestamp()
	s.updatedAt = now
	s.updatedStr = formatNaive(now)
}


// AppendMessagesDurable is the latency-first persistence path.
//
// Instead of rewriting the complete session JSONL before every provider call,
// it appends only the new message records to a journal sidecar under the same
// cross-process session lock. A later Save compacts the journal into the
// canonical JSONL. The method updates the in-memory transcript only after the
// append succeeds, so callers never observe a message that was not durably
// recorded.
func (s *Session) AppendMessagesDurable(messages []core.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if s.store == nil {
		return errors.New("session: session has no store")
	}
	if err := s.store.initErr; err != nil {
		return err
	}

	normalized := make([]core.Message, len(messages))
	copy(normalized, messages)
	for i := range normalized {
		if normalized[i].Timestamp == "" {
			normalized[i].Timestamp = formatNaive(nowTimestamp())
		}
	}
	var buf bytes.Buffer
	for i := range normalized {
		if err := encodeMessage(&buf, &normalized[i]); err != nil {
			return err
		}
		buf.WriteByte('\n')
	}

	return s.store.withLock(func() error {
		path := s.store.journalPath(s.key)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("session: open journal %s: %w", path, err)
		}
		if _, err := f.Write(buf.Bytes()); err != nil {
			_ = f.Close()
			return fmt.Errorf("session: append journal %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("session: close journal %s: %w", path, err)
		}

		s.mu.Lock()
		for _, m := range normalized {
			s.messages = append(s.messages, m)
			s.snapshots = append(s.snapshots, core.Message{})
			s.rawLines = append(s.rawLines, nil)
		}
		now := nowTimestamp()
		s.updatedAt = now
		s.updatedStr = formatNaive(now)
		s.mu.Unlock()

		// Refresh the composite base+journal cache fingerprint. Subsequent turns
		// stay entirely in memory unless another process changes either file.
		s.store.rememberCurrent(s)
		return nil
	})
}

// SetMessage replaces the message at index i.
//
// This is an extension: the reference mutates session.messages in place. A
// replaced message is re-encoded on the next Save rather than written back
// verbatim.
func (s *Session) SetMessage(i int, m core.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.messages) {
		return fmt.Errorf("session: message index %d out of range", i)
	}
	s.messages[i] = m
	s.snapshots[i] = core.Message{}
	s.rawLines[i] = nil
	return nil
}

// insertMessageLocked splices a message into the transcript at index i.
//
// Python's list.insert clamps its index rather than rejecting it, and this
// package has to reproduce that: commit_summary_checkpoint stores the RAW
// insert_at in last_archived while list.insert clamps it, so an out-of-range
// boundary is observable both in memory and on disk. A negative index counts
// from the end (and clamps at 0), an index past the end appends.
//
// The stored record is spliced in step with the message, so the byte-for-byte
// fidelity model in Session's doc comment keeps holding: passing a non-nil raw
// record lets the caller pin the exact bytes Python would have written,
// including key order, which core.Message's marshaller cannot express.
func (s *Session) insertMessageLocked(i int, m core.Message, raw []byte) {
	n := len(s.messages)
	if i < 0 {
		i += n
		if i < 0 {
			i = 0
		}
	}
	if i > n {
		i = n
	}

	s.messages = append(s.messages, core.Message{})
	copy(s.messages[i+1:], s.messages[i:])
	s.messages[i] = m

	s.snapshots = append(s.snapshots, core.Message{})
	copy(s.snapshots[i+1:], s.snapshots[i:])
	s.snapshots[i] = m

	s.rawLines = append(s.rawLines, nil)
	copy(s.rawLines[i+1:], s.rawLines[i:])
	if raw == nil {
		s.rawLines[i] = nil
	} else {
		s.rawLines[i] = append([]byte(nil), raw...)
	}
}

// setMetaRawLocked records a metadata value together with the exact JSON text
// Python would write for it.
//
// The key is appended to the insertion order when it is new, mirroring a
// Python dict assignment; an existing key keeps its position.
func (s *Session) setMetaRawLocked(key string, value any, raw json.RawMessage) {
	if _, exists := s.meta[key]; !exists {
		s.metaOrder = append(s.metaOrder, key)
	}
	if s.metaRaw == nil {
		s.metaRaw = map[string]json.RawMessage{}
	}
	if s.metaLoaded == nil {
		s.metaLoaded = map[string]any{}
	}
	s.meta[key] = value
	s.metaRaw[key] = append(json.RawMessage(nil), raw...)
	s.metaLoaded[key] = decodeJSONValue(raw)
}

// dropMetaLocked removes a metadata key, its insertion-order slot and its
// cached bytes, mirroring `metadata.pop(key, None)` followed by a later
// re-insertion landing at the END of a Python dict.
func (s *Session) dropMetaLocked(key string) {
	delete(s.meta, key)
	delete(s.metaRaw, key)
	delete(s.metaLoaded, key)
	for i, k := range s.metaOrder {
		if k == key {
			s.metaOrder = append(s.metaOrder[:i], s.metaOrder[i+1:]...)
			break
		}
	}
}

// Clear resets the transcript, for /new.
//
// Mirrors Session.clear (manager.py:477-483): messages, the archive offset and
// the provider state are reset, "_last_summary" is dropped from the metadata
// and updated_at is advanced. Note that, exactly like the reference,
// metadata["runtime_checkpoint"] is left in place.
//
// Dropping "_last_summary" also removes its insertion-order slot, because a
// Python dict that loses a key and gets it back places it last; leaving the
// stale slot behind would put the next committed summary back where the old one
// was.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = nil
	s.snapshots = nil
	s.rawLines = nil
	s.lastArchived = 0
	s.providerState = nil
	s.providerLine = nil
	s.providerSnap = nil
	s.dropMetaLocked(LastSummaryMetaKey)
	now := nowTimestamp()
	s.updatedAt = now
	s.updatedStr = formatNaive(now)
}

// Metadata returns the session's free-form metadata map.
//
// The map is live: callers may read and write it, and the next Save writes the
// result into the metadata record. Numbers loaded from a Python file are
// json.Number, not float64, so a float such as 1.0 is not turned into an int on
// the way back out. The map is not safe for concurrent use.
func (s *Session) Metadata() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// UpdatedAt returns the session's last-mutation time.
//
// Mirrors session.updated_at, which the reference uses as the runtime
// checkpoint fingerprint. A timestamp read from disk is returned in the host's
// local zone, because that is how it was written.
func (s *Session) UpdatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updatedAt
}

// CreatedAt returns the session's creation time.
func (s *Session) CreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt
}

// LastArchived returns the replay watermark into the transcript.
//
// Mirrors Session.last_archived (manager.py:303-310); the persisted field is
// the legacy name "last_consolidated", and both are written on every save.
func (s *Session) LastArchived() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastArchived
}

// SetLastArchived sets the replay watermark.
func (s *Session) SetLastArchived(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastArchived = n
}

// ProviderState returns the provider-private conversation state read from the
// provider_state record, or nil when the session has none.
func (s *Session) ProviderState() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerState
}

// SetProviderState replaces the provider-private conversation state. Passing
// nil removes the record on the next save.
func (s *Session) SetProviderState(raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(raw) == 0 {
		s.providerState = nil
		return
	}
	s.providerState = raw
}

// CompactJournal folds an append-only journal into the canonical JSONL only
// when it has reached minBytes. The file lock is acquired before checking size
// and held through saveLocked, so a concurrent AppendMessagesDurable can never
// be lost between snapshot and journal removal.
func (s *Session) CompactJournal(minBytes int64) error {
	if s.store == nil {
		return errors.New("session: session has no store")
	}
	if err := s.store.initErr; err != nil {
		return err
	}
	return s.store.withLock(func() error {
		info, err := os.Stat(s.store.journalPath(s.key))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if minBytes > 0 && info.Size() < minBytes {
			return nil
		}
		return s.saveLocked()
	})
}

// Save writes the whole session to disk.
//
// Mirrors _save_unlocked (manager.py:1314-1361): the metadata record comes
// first and is regenerated on every save, the provider_state record follows
// when present, then every message; the file is replaced atomically and the
// runtime-checkpoint sidecar is unlinked. Like the reference's default
// (fsync=False), nothing is fsynced.
//
// Save holds the session-files lock for the whole operation, so concurrent
// Save calls — from several goroutines or from a Python process using
// filelock — cannot interleave and corrupt the file.
func (s *Session) Save() error {
	if s.store == nil {
		return errors.New("session: session has no store")
	}
	if err := s.store.initErr; err != nil {
		return err
	}
	return s.store.withLock(s.saveLocked)
}

// saveLocked renders and writes the session file. The caller must hold the
// session-files lock.
func (s *Session) saveLocked() error {
	s.mu.Lock()
	key := s.key
	messages := append([]core.Message(nil), s.messages...)
	snapshots := append([]core.Message(nil), s.snapshots...)
	rawLines := make([][]byte, len(s.rawLines))
	copy(rawLines, s.rawLines)
	meta := make(map[string]any, len(s.meta))
	for k, v := range s.meta {
		meta[k] = v
	}
	metaOrder := append([]string(nil), s.metaOrder...)
	metaExtra := append([]rawField(nil), s.metaExtra...)
	metaRaw := make(map[string]json.RawMessage, len(s.metaRaw))
	for k, v := range s.metaRaw {
		metaRaw[k] = v
	}
	metaLoaded := make(map[string]any, len(s.metaLoaded))
	for k, v := range s.metaLoaded {
		metaLoaded[k] = v
	}
	created := s.createdStr
	updated := s.updatedStr
	lastArchived := s.lastArchived
	providerState := s.providerState
	providerLine := s.providerLine
	providerSnap := s.providerSnap
	s.mu.Unlock()

	var buf bytes.Buffer

	// Metadata record (manager.py:1320-1331). Key order is Python's.
	buf.WriteString(`{"_type": "metadata"`)
	buf.WriteString(pyItemSep)
	writePyString(&buf, "key")
	buf.WriteString(pyKeySep)
	writePyString(&buf, key)
	buf.WriteString(pyItemSep)
	writePyString(&buf, "created_at")
	buf.WriteString(pyKeySep)
	writePyString(&buf, created)
	buf.WriteString(pyItemSep)
	writePyString(&buf, "updated_at")
	buf.WriteString(pyKeySep)
	writePyString(&buf, updated)
	buf.WriteString(pyItemSep)
	writePyString(&buf, "metadata")
	buf.WriteString(pyKeySep)
	if err := appendMetaMap(&buf, meta, metaOrder, metaRaw, metaLoaded); err != nil {
		return err
	}
	buf.WriteString(pyItemSep)
	writePyString(&buf, "last_archived")
	buf.WriteString(pyKeySep)
	buf.WriteString(strconv.Itoa(lastArchived))
	buf.WriteString(pyItemSep)
	writePyString(&buf, "last_consolidated")
	buf.WriteString(pyKeySep)
	buf.WriteString(strconv.Itoa(lastArchived))
	// Members this build does not model are preserved, in their file order.
	for _, f := range metaExtra {
		buf.WriteString(pyItemSep)
		writePyString(&buf, f.key)
		buf.WriteString(pyKeySep)
		if err := appendPyRaw(&buf, f.val); err != nil {
			return err
		}
	}
	buf.WriteString("}\n")

	// Provider state record (manager.py:1332-1337).
	if len(providerState) > 0 {
		if providerLine != nil && bytes.Equal(providerState, providerSnap) {
			buf.Write(providerLine)
			buf.WriteByte('\n')
		} else {
			buf.WriteString(`{"_type": "provider_state"`)
			buf.WriteString(pyItemSep)
			writePyString(&buf, "state")
			buf.WriteString(pyKeySep)
			if err := appendPyRaw(&buf, providerState); err != nil {
				return err
			}
			buf.WriteString("}\n")
		}
	}

	// Message records (manager.py:1338-1339).
	for i := range messages {
		if i < len(rawLines) && rawLines[i] != nil && i < len(snapshots) &&
			reflect.DeepEqual(messages[i], snapshots[i]) {
			buf.Write(rawLines[i])
			buf.WriteByte('\n')
			continue
		}
		if err := encodeMessage(&buf, &messages[i]); err != nil {
			return err
		}
		buf.WriteByte('\n')
	}

	path := s.store.Path(key)
	if err := writeAtomic(path, buf.Bytes(), 0o666); err != nil {
		return fmt.Errorf("session: write %s: %w", path, err)
	}
	// A full save supersedes both volatile sidecars. Because Save holds the
	// same session-files lock as AppendMessagesDurable, no newer journal append
	// can be deleted by this compaction.
	if err := os.Remove(s.store.checkpointPath(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session: remove checkpoint: %w", err)
	}
	if err := os.Remove(s.store.journalPath(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session: remove journal: %w", err)
	}
	s.store.rememberSaved(s)
	return nil
}

// encodeMessage writes one message record with Python's rules.
//
// core.Message's own marshaller provides the key order and the raw values of
// unmodelled fields; this function rewrites those bytes so escaping, separators
// and numbers match CPython, and translates the two fields whose Go shape
// differs from the persisted shape:
//
//   - tool_calls: Go models a flat {"id","name","arguments"} call, while
//     Python persists the OpenAI shape produced by to_openai_tool_call
//     (providers/base.py:87-104):
//     {"id", "type": "function", "function": {"name", "arguments"}}.
//   - content: a Go zero value marshals as null, which Python never writes
//     (build_assistant_message uses content or "", helpers.py:705).
func encodeMessage(buf *bytes.Buffer, m *core.Message) error {
	raw, err := m.MarshalJSON()
	if err != nil {
		return fmt.Errorf("session: marshal message: %w", err)
	}
	fields, err := objectFields(raw)
	if err != nil {
		return fmt.Errorf("session: marshal message: %w", err)
	}
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteString(pyItemSep)
		}
		writePyString(buf, f.key)
		buf.WriteString(pyKeySep)
		switch f.key {
		case "tool_calls":
			if len(m.ToolCalls) > 0 {
				if err := encodeToolCalls(buf, m.ToolCalls); err != nil {
					return err
				}
				continue
			}
			if err := appendPyRaw(buf, f.val); err != nil {
				return err
			}
		case "content":
			if bytes.Equal(bytes.TrimSpace(f.val), []byte("null")) {
				buf.WriteString(`""`)
				continue
			}
			if err := appendPyRaw(buf, f.val); err != nil {
				return err
			}
		default:
			if err := appendPyRaw(buf, f.val); err != nil {
				return err
			}
		}
	}
	buf.WriteByte('}')
	return nil
}

// encodeToolCalls writes Go tool calls in the persisted OpenAI shape.
func encodeToolCalls(buf *bytes.Buffer, calls []core.ToolCall) error {
	buf.WriteByte('[')
	for i := range calls {
		if i > 0 {
			buf.WriteString(pyItemSep)
		}
		call := &calls[i]
		buf.WriteByte('{')
		writePyString(buf, "id")
		buf.WriteString(pyKeySep)
		writePyString(buf, call.ID)
		buf.WriteString(pyItemSep)
		writePyString(buf, "type")
		buf.WriteString(pyKeySep)
		writePyString(buf, "function")
		buf.WriteString(pyItemSep)
		writePyString(buf, "function")
		buf.WriteString(pyKeySep)
		buf.WriteByte('{')
		writePyString(buf, "name")
		buf.WriteString(pyKeySep)
		writePyString(buf, call.Name)
		buf.WriteString(pyItemSep)
		writePyString(buf, "arguments")
		buf.WriteString(pyKeySep)
		writePyString(buf, toolCallArguments(call))
		if len(call.FunctionProviderSpecific) > 0 {
			buf.WriteString(pyItemSep)
			writePyString(buf, "provider_specific_fields")
			buf.WriteString(pyKeySep)
			if err := appendPyValue(buf, call.FunctionProviderSpecific); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		if len(call.ExtraContent) > 0 {
			buf.WriteString(pyItemSep)
			writePyString(buf, "extra_content")
			buf.WriteString(pyKeySep)
			if err := appendPyValue(buf, call.ExtraContent); err != nil {
				return err
			}
		}
		if len(call.ProviderSpecificFields) > 0 {
			buf.WriteString(pyItemSep)
			writePyString(buf, "provider_specific_fields")
			buf.WriteString(pyKeySep)
			if err := appendPyValue(buf, call.ProviderSpecificFields); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	}
	buf.WriteByte(']')
	return nil
}

// toolCallArguments renders arguments as the JSON string Python persists.
//
// to_openai_tool_call (providers/base.py:87-91) keeps a str argument as-is and
// json.dumps anything else, so the persisted value is always a string. Go keeps
// the raw JSON value instead, so a JSON string literal is unwrapped and any
// other value is passed through as its own text.
func toolCallArguments(call *core.ToolCall) string {
	raw := bytes.TrimSpace(call.Arguments)
	if len(raw) == 0 {
		return "{}"
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return string(raw)
}

// consumeRecords parses the records of a session file into the session.
func (s *Session) consumeRecords(data []byte) error {
	for _, line := range splitLines(data) {
		trimmed := bytes.TrimSpace([]byte(line))
		if len(trimmed) == 0 {
			continue
		}
		fields, err := objectFields(trimmed)
		if err != nil {
			// The reference's repair path skips lines that are not JSON
			// objects and leaves the file untouched (manager.py:1140-1147).
			continue
		}
		recordType, _ := stringField(fields, "_type")
		switch recordType {
		case "metadata":
			s.applyMetadataRecord(fields)
		case "provider_state":
			s.applyProviderStateRecord(fields, trimmed)
		default:
			s.appendMessageRecord(fields, trimmed)
		}
	}
	return nil
}

// applyMetadataRecord applies one metadata record; the last one wins, exactly
// as in _load_unlocked (manager.py:1064-1084).
func (s *Session) applyMetadataRecord(fields []rawField) {
	if raw, ok := fieldValue(fields, "metadata"); ok {
		if meta, order, ok := decodeOrderedObject(raw); ok {
			s.meta = meta
			s.metaOrder = order
			s.captureMetaRaw(raw)
		} else {
			s.resetMeta()
		}
	} else {
		s.resetMeta()
	}

	if raw, ok := fieldValue(fields, "created_at"); ok {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil && str != "" {
			if t, normalized, ok := parseTimestamp(str); ok {
				s.createdAt = t
				s.createdStr = normalized
			}
		}
	}
	if raw, ok := fieldValue(fields, "updated_at"); ok {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil && str != "" {
			if t, normalized, ok := parseTimestamp(str); ok {
				s.updatedAt = t
				s.updatedStr = normalized
			}
		}
	}

	s.lastArchived = archiveOffset(fields)

	extras := make([]rawField, 0, len(fields))
	for _, f := range fields {
		switch f.key {
		case "_type", "key", "created_at", "updated_at", "metadata",
			"last_archived", "last_consolidated":
		default:
			extras = append(extras, f)
		}
	}
	s.metaExtra = extras
}

// resetMeta drops every metadata value and its cached bytes.
func (s *Session) resetMeta() {
	s.meta = map[string]any{}
	s.metaOrder = nil
	s.metaRaw = map[string]json.RawMessage{}
	s.metaLoaded = map[string]any{}
}

// captureMetaRaw records the exact JSON text of every metadata value in raw,
// alongside an independent decode of those same bytes.
//
// The two are decoded separately (rather than sharing one structure) so that
// Save can detect an in-place mutation of a nested value: metaLoaded must not
// alias anything the caller can reach through Metadata().
func (s *Session) captureMetaRaw(raw json.RawMessage) {
	s.metaRaw = map[string]json.RawMessage{}
	s.metaLoaded = map[string]any{}
	fields, err := objectFields(raw)
	if err != nil {
		return
	}
	for _, f := range fields {
		s.metaRaw[f.key] = append(json.RawMessage(nil), f.val...)
		s.metaLoaded[f.key] = decodeJSONValue(f.val)
	}
}

// archiveOffset mirrors _archive_offset (manager.py:86-92): "last_archived"
// wins, "last_consolidated" is the fallback, a bool is rejected and anything
// else yields 0.
func archiveOffset(fields []rawField) int {
	for _, key := range []string{"last_archived", "last_consolidated"} {
		if v, ok := intField(fields, key); ok {
			return v
		}
	}
	return 0
}

// applyProviderStateRecord applies one provider_state record. An invalid state
// is treated as absent, which is what the load path does when
// from_private_record returns None (manager.py:1085-1088).
func (s *Session) applyProviderStateRecord(fields []rawField, line []byte) {
	raw, ok := fieldValue(fields, "state")
	if !ok || !validProviderState(raw) {
		s.providerState = nil
		s.providerLine = nil
		s.providerSnap = nil
		return
	}
	s.providerState = raw
	s.providerLine = append([]byte(nil), line...)
	s.providerSnap = raw
}

// validProviderState mirrors ProviderConversationState.from_private_record
// (providers/base.py:209-247).
func validProviderState(raw json.RawMessage) bool {
	fields, err := objectFields(raw)
	if err != nil {
		return false
	}
	for _, key := range []string{"kind", "provider", "model"} {
		value, ok := stringField(fields, key)
		if !ok || value == "" {
			return false
		}
	}
	if _, ok := intField(fields, "version"); !ok {
		return false
	}
	if payload, ok := fieldValue(fields, "payload"); !ok || !isJSONObject(payload) {
		return false
	}
	pending, ok := fieldValue(fields, "pending_messages")
	if !ok {
		// data.get("pending_messages", []) defaults to an empty list, which is
		// a list of dicts and therefore valid (providers/base.py:223).
		return true
	}
	items, err := arrayItems(pending)
	if err != nil {
		return false
	}
	for _, item := range items {
		if !isJSONObject(item) {
			return false
		}
	}
	return true
}

// isJSONObject reports whether raw is a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	_, err := objectFields(raw)
	return err == nil
}

// arrayItems decodes a JSON array into its raw elements.
func arrayItems(raw json.RawMessage) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, errors.New("session: not a JSON array")
	}
	var items []json.RawMessage
	for dec.More() {
		var item json.RawMessage
		if err := dec.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return items, nil
}

// appendMessageRecord appends one message record.
//
// Any record whose "_type" is neither "metadata" nor "provider_state" is a
// message (manager.py:1089-1090), including one that carries an unknown future
// "_type": it is kept verbatim and re-emitted unchanged on save.
func (s *Session) appendMessageRecord(fields []rawField, line []byte) {
	var msg core.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		// A JSON object that core.Message cannot model (for example a
		// non-string "role") is still a message for the reference, which does
		// not validate records. Keep it opaque rather than dropping it.
		msg = core.Message{}
	}
	if raw, ok := fieldValue(fields, "tool_calls"); ok {
		if calls, ok := convertToolCalls(raw); ok {
			msg.ToolCalls = calls
		}
	}
	s.messages = append(s.messages, msg)
	s.snapshots = append(s.snapshots, msg)
	s.rawLines = append(s.rawLines, append([]byte(nil), line...))
}

// convertToolCalls rebuilds core.ToolCall values from a persisted tool_calls
// array.
//
// core.Message.UnmarshalJSON decodes tool_calls straight into []core.ToolCall,
// whose fields are flat; Python persists the OpenAI shape, whose name and
// arguments live under "function". Without this translation a Python-written
// assistant turn would replay with an empty tool name and no arguments.
func convertToolCalls(raw json.RawMessage) ([]core.ToolCall, bool) {
	items, err := arrayItems(raw)
	if err != nil {
		return nil, false
	}
	calls := make([]core.ToolCall, 0, len(items))
	for _, item := range items {
		fields, err := objectFields(item)
		if err != nil {
			return nil, false
		}
		var call core.ToolCall
		if id, ok := stringField(fields, "id"); ok {
			call.ID = id
		}
		name, hasName := stringField(fields, "name")
		arguments, hasArguments := fieldValue(fields, "arguments")
		if fn, ok := fieldValue(fields, "function"); ok {
			fnFields, err := objectFields(fn)
			if err != nil {
				return nil, false
			}
			if v, ok := stringField(fnFields, "name"); ok {
				name, hasName = v, true
			}
			if v, ok := fieldValue(fnFields, "arguments"); ok {
				arguments, hasArguments = v, true
			}
			if v, ok := fieldValue(fnFields, "provider_specific_fields"); ok {
				_ = json.Unmarshal(v, &call.FunctionProviderSpecific)
			}
		}
		if hasName {
			call.Name = name
		}
		if hasArguments {
			call.Arguments = normalizeToolArguments(arguments)
		}
		if v, ok := fieldValue(fields, "extra_content"); ok {
			_ = json.Unmarshal(v, &call.ExtraContent)
		}
		if v, ok := fieldValue(fields, "provider_specific_fields"); ok {
			_ = json.Unmarshal(v, &call.ProviderSpecificFields)
		}
		calls = append(calls, call)
	}
	return calls, true
}

// normalizeToolArguments converts a persisted arguments value into the raw JSON
// value Go providers expect.
//
// Python persists the argument JSON as a string (or, rarely, as an object);
// core.ToolCall.Arguments holds the parsed value, so a string literal is
// unwrapped. A string that is not valid JSON is left as the literal, which is
// what parse_tool_arguments would hand back to a tool.
// normalizeToolArguments unwraps a string-encoded argument payload. The
// implementation is shared with core so the two cannot drift.
func normalizeToolArguments(raw json.RawMessage) json.RawMessage {
	return core.NormalizeToolArguments(raw)
}

// overlayCheckpointLocked applies the runtime-checkpoint sidecar, mirroring
// _overlay_runtime_checkpoint_unlocked (manager.py:1262-1312).
//
// A sidecar that fails validation is unlinked, exactly like the reference,
// because atomic writes mean a malformed sidecar can never become valid.
func (s *Store) overlayCheckpointLocked(sess *Session, mainPath string) {
	path := s.checkpointPath(sess.key)
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if !info.Mode().IsRegular() {
		// A symlink or directory is ignored without being deleted
		// (manager.py:1265-1271).
		return
	}
	mainInfo, err := os.Stat(mainPath)
	if err != nil {
		return
	}
	// A complete session save supersedes an older sidecar; this closes the
	// crash window between replacing the JSONL and unlinking the sidecar.
	if mainInfo.ModTime().UnixNano() > info.ModTime().UnixNano() {
		_ = os.Remove(path)
		return
	}

	discard := func() {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(path)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		discard()
		return
	}
	fields, err := objectFields(data)
	if err != nil {
		discard()
		return
	}
	version, ok := intField(fields, "version")
	if !ok || version != 1 {
		discard()
		return
	}
	checkpointKey, ok := stringField(fields, "session_key")
	if !ok || checkpointKey != sess.key {
		discard()
		return
	}
	baseUpdated, ok := stringField(fields, "base_updated_at")
	if !ok || baseUpdated != sess.updatedStr {
		discard()
		return
	}
	baseCount, ok := intField(fields, "base_message_count")
	if !ok || baseCount != len(sess.messages) {
		discard()
		return
	}
	checkpointRaw, ok := fieldValue(fields, "checkpoint")
	if !ok {
		discard()
		return
	}
	checkpoint, order, ok := decodeOrderedObject(checkpointRaw)
	if !ok {
		discard()
		return
	}

	providerRaw, hasProvider := fieldValue(fields, "provider_state")
	if hasProvider && !bytes.Equal(bytes.TrimSpace(providerRaw), []byte("null")) {
		if !validProviderState(providerRaw) {
			discard()
			return
		}
		sess.providerState = providerRaw
		sess.providerLine = nil
		sess.providerSnap = providerRaw
	}

	if sess.meta == nil {
		sess.meta = map[string]any{}
	}
	if _, exists := sess.meta["runtime_checkpoint"]; !exists {
		sess.metaOrder = append(sess.metaOrder, "runtime_checkpoint")
	}
	// The sidecar's checkpoint value is re-emitted byte for byte on the next
	// save, which is what the reference does: it assigns the decoded dict into
	// the metadata and json.dumps writes it back in the order it was loaded.
	sess.setMetaRawLocked("runtime_checkpoint", checkpoint, checkpointRaw)
	_ = order
}
