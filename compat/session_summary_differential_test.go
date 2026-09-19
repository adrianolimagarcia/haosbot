package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

// Differential tests for the session summary / checkpoint machinery.
//
// This file is deliberately self-contained: its own dumper
// (compat/python/dump_session_summary.py) and its own loader. It shares no
// declarations with differential_test.go, manager_differential_test.go or the
// other compat tests beyond repoRootForSessionSummary, because those files are
// edited concurrently by several agents and a read-modify-write race there
// would destroy work.
//
// The dumper drives the REAL frozen reference: the real Session, the real
// JsonlSessionStore (so sessions are seeded as files and loaded through the
// reference's own load path), the real get_history and the real
// commit_summary_checkpoint. Nothing here is transcribed from documentation.
//
// Two normalisations are applied to BOTH sides before comparing, and both are
// forced by the port's message model rather than by the session logic:
//
//  1. tool_calls. The reference replays the persisted OpenAI-shaped value
//     verbatim; core.ToolCall is a flat {id, name, arguments} model, so both
//     sides are canonicalised to {id, name, arguments} with a JSON-string
//     argument payload parsed into its value.
//  2. A null-valued optional key. The reference emits {"tool_calls": null} when
//     the persisted key is present and null; core.Message's marshaller drops a
//     nil optional field. Both sides therefore drop null-valued optional keys.
//     This is reported, not hidden: the counts of affected cases are asserted
//     to be zero for this corpus below.
//
// The continuation marker's timestamp is datetime.now().isoformat() in the
// reference and time.Now() here, so it can never match. Both sides replace it
// with "<MARKER_TS>" and the shape is asserted separately.

const sessionSummaryContinuation = "Continue the active task from the working-memory checkpoint above."

var sessionSummaryMarkerTSRe = regexp.MustCompile(
	`("content": "Continue the active task from the working-memory checkpoint above\.", ` +
		`"_hidden_history": true, "timestamp": ")[^"]*(")`)

var sessionSummaryISOTSRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{6})?$`)

func normalizeSessionSummaryPersisted(text string) string {
	return sessionSummaryMarkerTSRe.ReplaceAllString(text, "${1}<MARKER_TS>${2}")
}

// ---------------------------------------------------------------------------
// Dumper plumbing
// ---------------------------------------------------------------------------

func repoRootForSessionSummary() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(wd) // compat/ -> repository root
}

var (
	sessionSummaryDocOnce sync.Once
	sessionSummaryDoc     map[string]any
	sessionSummaryErr     error

	// sessionSummarySkip is set when the reference venv is absent. It is a
	// sentinel rather than an error so every caller can t.Skip instead of
	// t.Fatalf, and so the skip survives the sync.Once for later tests.
	sessionSummarySkip string
)

func loadSessionSummaryDump(t *testing.T) map[string]any {
	t.Helper()
	sessionSummaryDocOnce.Do(func() {
		root := repoRootForSessionSummary()
		python := filepath.Join(root, ".tools", "venv", "bin", "python")
		script := filepath.Join(root, "compat", "python", "dump_session_summary.py")
		if _, err := os.Stat(python); err != nil {
			// A missing reference venv means the differential check cannot run at
			// all, which is a SKIP - exactly like the other compat suites. It used
			// to be recorded as an error and turned into t.Fatalf below, so running
			// ./compat anywhere without the venv (every git worktree, for instance)
			// reported four hard failures instead of four skips, which reads like a
			// broken port rather than a missing harness.
			sessionSummarySkip = fmt.Sprintf(
				"SKIP: reference venv not present at %s - differential check not run", python)
			return
		}
		if _, err := os.Stat(script); err != nil {
			sessionSummaryErr = fmt.Errorf("dumper missing at %s", script)
			return
		}
		out, err := runReferenceCommand("session-summary dumper", []string{python, script}, root, nil)
		if err != nil {
			sessionSummaryErr = fmt.Errorf("dumper failed: %w", err)
			return
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		if err := dec.Decode(&sessionSummaryDoc); err != nil {
			sessionSummaryErr = fmt.Errorf("dumper output is not valid JSON: %w", err)
			return
		}
	})
	if sessionSummarySkip != "" {
		t.Skip(sessionSummarySkip)
	}
	if sessionSummaryErr != nil {
		t.Fatalf("%v", sessionSummaryErr)
	}
	return sessionSummaryDoc
}

func sessionSummarySection(t *testing.T, doc map[string]any, name string) []any {
	t.Helper()
	raw, ok := doc[name].([]any)
	if !ok {
		t.Fatalf("dumper section %q is missing or not a list", name)
	}
	if len(raw) == 0 {
		t.Fatalf("dumper section %q is empty", name)
	}
	return raw
}

func sessionSummaryString(t *testing.T, doc map[string]any, name string) string {
	t.Helper()
	value, ok := doc[name].(string)
	if !ok || value == "" {
		t.Fatalf("dumper field %q is missing or not a string", name)
	}
	return value
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// decodeJSONValue decodes a JSON value the way the session store does, so
// numbers keep the int/float distinction the reference's json.loads gives them.
func ssDecodeJSON(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// seededStore writes the reference-produced session file into a fresh store
// and opens it through the store's own load path.
func seededStore(t *testing.T, key string, seedLines []any) (*session.Store, *session.Session) {
	t.Helper()
	store := session.NewStore("", t.TempDir())
	path := store.Path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := make([]string, 0, len(seedLines))
	for _, line := range seedLines {
		text, ok := line.(string)
		if !ok {
			t.Fatalf("seed line is not a string: %T", line)
		}
		lines = append(lines, text)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o666); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	sess, err := store.Open(key)
	if err != nil {
		t.Fatalf("open seeded session: %v", err)
	}
	return store, sess
}

// decodeMessage decodes a dumped message object into core.Message. A message
// core.Message cannot model (a non-string role, a numeric content) decodes to
// the zero Message, which is exactly what the store's record reader does.
func ssDecodeMessage(t *testing.T, raw any) core.Message {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	var m core.Message
	_ = json.Unmarshal(encoded, &m)
	return m
}

// asMap converts a decoded JSON value into a map, failing the test otherwise.
func ssMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value is not a JSON object: %T", v)
	}
	return m
}

// ---------------------------------------------------------------------------
// 1. session_summary_from_metadata (which also exercises datetime.fromisoformat)
// ---------------------------------------------------------------------------

func TestSessionSummaryFromMetadataDifferential(t *testing.T) {
	doc := loadSessionSummaryDump(t)
	cases := sessionSummarySection(t, doc, "summary_metadata")
	fallbackText := sessionSummaryString(t, doc, "fallback_last_active")
	fallback := parseFallbackLastActive(t, fallbackText)

	// The dumper's fromisoformat section must agree with what
	// session_summary_from_metadata did with the same string, otherwise the
	// corpus is not exercising what it claims to.
	//
	// The dumper nests the corpus as {"accepted": N, "cases": [...]}; the list is
	// what maps a value to its datetime.fromisoformat verdict. Reading the section
	// as a list directly failed before a single case was compared, so the whole
	// test was reporting a harness-shape error rather than a port divergence.
	isoSection := ssMap(t, doc["fromisoformat"])
	isoCases, ok := isoSection["cases"].([]any)
	if !ok {
		t.Fatalf("dumper section fromisoformat.cases is missing or not a list")
	}
	isoByValue := map[string]bool{}
	isoAccepted := 0
	for _, raw := range isoCases {
		entry := ssMap(t, raw)
		value, _ := entry["s"].(string)
		acceptedValue, _ := entry["ok"].(bool)
		isoByValue[value] = acceptedValue
		if acceptedValue {
			isoAccepted++
		}
	}
	if declared, err := ssJSONInt(isoSection["accepted"]); err != nil || declared != isoAccepted {
		t.Errorf("dumper fromisoformat accepted = %v, but the corpus contains %d accepted values",
			isoSection["accepted"], isoAccepted)
	}

	accepted, rejected, nonNil := 0, 0, 0
	for _, raw := range cases {
		entry := ssMap(t, raw)
		id, _ := entry["id"].(string)

		var metadata map[string]any
		if entry["metadata"] != nil {
			metadata = ssMap(t, entry["metadata"])
		}

		got := session.SessionSummaryFromMetadata(metadata, fallback)

		want := entry["result"]
		if want == nil {
			if got != nil {
				t.Errorf("%s: got %+v, want nil", id, got)
			}
			continue
		}
		wantMap := ssMap(t, want)
		if got == nil {
			t.Errorf("%s: got nil, want %v", id, wantMap)
			continue
		}
		nonNil++
		if got.Text != wantMap["text"] {
			t.Errorf("%s: text = %q, want %q", id, got.Text, wantMap["text"])
		}
		if got.LastActive != wantMap["last_active"] {
			t.Errorf("%s: last_active = %q, want %q", id, got.LastActive, wantMap["last_active"])
		}

		// Cross-check the accept/reject decision against the fromisoformat
		// corpus whenever this case is one of its members.
		if strings.HasPrefix(id, "last_active_corpus_") {
			value, _ := ssMap(t, entry["metadata"])["_last_summary"].(map[string]any)["last_active"].(string)
			if isoByValue[value] {
				accepted++
				if got.LastActive != value {
					t.Errorf("%s: fromisoformat accepted %q but the port replaced it with %q",
						id, value, got.LastActive)
				}
			} else {
				rejected++
				if got.LastActive != fallbackText {
					t.Errorf("%s: fromisoformat rejected %q but the port kept %q",
						id, value, got.LastActive)
				}
			}
		}
	}

	if len(cases) == 0 || nonNil == 0 {
		t.Fatalf("no cases exercised: cases=%d nonNil=%d", len(cases), nonNil)
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("fromisoformat corpus is not discriminating: accepted=%d rejected=%d", accepted, rejected)
	}
	t.Logf("session_summary_from_metadata: %d cases (%d non-nil, %d last_active accepted, %d rejected)",
		len(cases), nonNil, accepted, rejected)
}

func parseFallbackLastActive(t *testing.T, s string) time.Time {
	t.Helper()
	// The reference emits the fallback as datetime.isoformat() of a naive local
	// datetime, which is the shape the session store's formatNaive produces.
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.Local)
	if err != nil {
		t.Fatalf("fallback_last_active %q is not parseable: %v", s, err)
	}
	return parsed
}

// ---------------------------------------------------------------------------
// 2. is_summary_checkpoint
// ---------------------------------------------------------------------------

func TestIsSummaryCheckpointDifferential(t *testing.T) {
	doc := loadSessionSummaryDump(t)
	cases := sessionSummarySection(t, doc, "is_summary_checkpoint")

	trueCount, falseCount := 0, 0
	for _, raw := range cases {
		entry := ssMap(t, raw)
		id, _ := entry["id"].(string)
		want, ok := entry["result"].(bool)
		if !ok {
			t.Fatalf("%s: dumper result is not a bool", id)
		}
		message := ssDecodeMessage(t, entry["message"])
		got := session.IsSummaryCheckpoint(message)
		if got != want {
			t.Errorf("%s: IsSummaryCheckpoint = %v, want %v (message %v)",
				id, got, want, entry["message"])
		}
		if want {
			trueCount++
		} else {
			falseCount++
		}
	}
	if len(cases) == 0 || trueCount == 0 || falseCount == 0 {
		t.Fatalf("is_summary_checkpoint corpus is not discriminating: %d cases, %d true, %d false",
			len(cases), trueCount, falseCount)
	}
	t.Logf("is_summary_checkpoint: %d cases (%d true, %d false)", len(cases), trueCount, falseCount)
}

// ---------------------------------------------------------------------------
// 3. commit_summary_checkpoint, including the persisted on-disk bytes
// ---------------------------------------------------------------------------

func TestCommitSummaryCheckpointDifferential(t *testing.T) {
	doc := loadSessionSummaryDump(t)
	cases := sessionSummarySection(t, doc, "commit")
	key := sessionSummaryString(t, doc, "key")

	persistedCompared, markerShapeChecked := 0, 0
	for _, raw := range cases {
		entry := ssMap(t, raw)
		id, _ := entry["id"].(string)
		summary, _ := entry["summary"].(string)
		seedLines := sessionSummarySection(t, entry, "seed_lines")

		var insertAt *int
		if entry["insert_at"] != nil {
			n, err := ssJSONInt(entry["insert_at"])
			if err != nil {
				t.Fatalf("%s: insert_at: %v", id, err)
			}
			insertAt = &n
		}
		var lastActive *time.Time
		if entry["last_active"] != nil {
			text, _ := entry["last_active"].(string)
			parsed, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", text, time.Local)
			if err != nil {
				t.Fatalf("%s: last_active %q: %v", id, text, err)
			}
			lastActive = &parsed
		}

		store, sess := seededStore(t, key, seedLines)
		sess.CommitSummaryCheckpoint(summary, insertAt, lastActive)
		if err := sess.Save(); err != nil {
			t.Fatalf("%s: save: %v", id, err)
		}

		// (a) In-memory transcript, marker timestamp normalised.
		//
		// The normalisation must be applied to BOTH sides. The marker's timestamp
		// comes from datetime.now() in the reference and from the Go clock here,
		// so leaving the Go side raw compared a live timestamp against the
		// dumper's "<MARKER_TS>" placeholder and failed on every case.
		rawMessages := ssMarshalMessages(t, sess.Messages())
		for _, m := range rawMessages {
			fields := m.(map[string]any)
			if fields["content"] != sessionSummaryContinuation {
				continue
			}
			ts, _ := fields["timestamp"].(string)
			if !sessionSummaryISOTSRe.MatchString(ts) {
				t.Errorf("%s: marker timestamp %q is not datetime.isoformat() shaped", id, ts)
			}
			markerShapeChecked++
		}
		gotMessages := ssNormalizeMarkerTimestamps(t, rawMessages)
		wantMessages := ssNormalizeMarkerTimestamps(t, sessionSummarySection(t, entry, "messages"))
		if !reflect.DeepEqual(gotMessages, wantMessages) {
			t.Errorf("%s: messages differ\n got: %s\nwant: %s",
				id, ssJSONString(gotMessages), ssJSONString(wantMessages))
		}

		// (b) last_archived, including the unclamped out-of-range values.
		wantArchived, err := ssJSONInt(entry["last_archived"])
		if err != nil {
			t.Fatalf("%s: last_archived: %v", id, err)
		}
		if got := sess.LastArchived(); got != wantArchived {
			t.Errorf("%s: last_archived = %d, want %d", id, got, wantArchived)
		}

		// (c) metadata, "_last_summary" included.
		wantMeta := ssMap(t, entry["metadata"])
		gotMeta := ssDecodeJSON(t, ssJSONBytes(t, sess.Metadata()))
		if !reflect.DeepEqual(gotMeta, wantMeta) {
			t.Errorf("%s: metadata differ\n got: %s\nwant: %s",
				id, ssJSONString(gotMeta), ssJSONString(wantMeta))
		}

		// (d) The persisted file, byte for byte after the one normalisation.
		onDisk, err := os.ReadFile(store.Path(key))
		if err != nil {
			t.Fatalf("%s: read persisted session: %v", id, err)
		}
		gotPersisted := normalizeSessionSummaryPersisted(string(onDisk))
		wantPersisted, _ := entry["persisted"].(string)
		if wantPersisted == "" {
			t.Fatalf("%s: dumper did not emit persisted bytes", id)
		}
		if gotPersisted != wantPersisted {
			t.Errorf("%s: persisted bytes differ\n--- got ---\n%s\n--- want ---\n%s",
				id, gotPersisted, wantPersisted)
		}
		persistedCompared++
	}

	if len(cases) == 0 || persistedCompared == 0 || markerShapeChecked == 0 {
		t.Fatalf("commit corpus is not discriminating: cases=%d persisted=%d markers=%d",
			len(cases), persistedCompared, markerShapeChecked)
	}
	t.Logf("commit_summary_checkpoint: %d cases (%d persisted byte comparisons, %d markers)",
		len(cases), persistedCompared, markerShapeChecked)
}

// ---------------------------------------------------------------------------
// 4. get_history
// ---------------------------------------------------------------------------

func TestGetHistoryDifferential(t *testing.T) {
	doc := loadSessionSummaryDump(t)
	cases := sessionSummarySection(t, doc, "get_history")
	key := sessionSummaryString(t, doc, "key")

	nonEmpty, droppedNulls := 0, 0
	for _, raw := range cases {
		entry := ssMap(t, raw)
		id, _ := entry["id"].(string)

		maxMessages, err := ssJSONInt(entry["max_messages"])
		if err != nil {
			t.Fatalf("%s: max_messages: %v", id, err)
		}
		extendToUser, _ := entry["extend_to_user"].(bool)
		includeRuntimeContext, _ := entry["include_runtime_context"].(bool)
		seedLines := sessionSummarySection(t, entry, "seed_lines")

		_, sess := seededStore(t, key, seedLines)
		got := sess.GetHistory(maxMessages, 0, extendToUser, includeRuntimeContext)

		wantRaw, ok := entry["result"].([]any)
		if !ok {
			t.Fatalf("%s: reference result is not a list: %v", id, entry["result"])
		}

		gotEntries := ssNormalizeHistoryEntries(t, ssMarshalMessages(t, got), &droppedNulls)
		wantEntries := ssNormalizeHistoryEntries(t, wantRaw, &droppedNulls)

		if !reflect.DeepEqual(gotEntries, wantEntries) {
			t.Errorf("%s: get_history differs\n got: %s\nwant: %s",
				id, ssJSONString(gotEntries), ssJSONString(wantEntries))
		}
		if len(got) > 0 {
			nonEmpty++
		}
	}

	if len(cases) == 0 || nonEmpty == 0 {
		t.Fatalf("get_history corpus is not discriminating: cases=%d nonEmpty=%d", len(cases), nonEmpty)
	}
	if droppedNulls != 0 {
		t.Errorf("the corpus hit the known null-optional-key divergence %d times; "+
			"those cases compare equal only because both sides drop the key", droppedNulls)
	}
	t.Logf("get_history: %d cases (%d non-empty results)", len(cases), nonEmpty)
}

// ---------------------------------------------------------------------------
// Comparison helpers
// ---------------------------------------------------------------------------

// marshalMessages renders a Go transcript as decoded JSON values.
func ssMarshalMessages(t *testing.T, messages []core.Message) []any {
	t.Helper()
	out := make([]any, 0, len(messages))
	for i := range messages {
		raw, err := messages[i].MarshalJSON()
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		out = append(out, ssDecodeJSON(t, raw))
	}
	return out
}

// normalizeMarkerTimestamps replaces the continuation marker's timestamp with
// the same placeholder the dumper used.
func ssNormalizeMarkerTimestamps(t *testing.T, messages []any) []any {
	t.Helper()
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message is not a JSON object: %T", raw)
		}
		if message["_hidden_history"] == true && message["content"] == sessionSummaryContinuation {
			message["timestamp"] = "<MARKER_TS>"
		}
		out = append(out, message)
	}
	return out
}

// normalizeHistoryEntries applies the two documented normalisations to a list
// of replayed entries.
func ssNormalizeHistoryEntries(t *testing.T, entries []any, droppedNulls *int) []any {
	t.Helper()
	out := make([]any, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("entry is not a JSON object: %T", raw)
		}
		normalized := map[string]any{"role": entry["role"]}
		content, hasContent := entry["content"]
		if !hasContent {
			content = nil
		}
		normalized["content"] = content

		for _, key := range []string{
			"tool_calls", "tool_call_id", "name", "reasoning_content", "thinking_blocks",
		} {
			value, present := entry[key]
			if !present {
				continue
			}
			if value == nil {
				*droppedNulls++
				continue
			}
			if key == "tool_calls" {
				value = ssCanonicalToolCalls(t, value)
			}
			normalized[key] = value
		}
		out = append(out, normalized)
	}
	return out
}

// canonicalToolCalls reduces both the persisted OpenAI shape and the port's
// flat shape to {id, name, arguments}, with a string argument payload parsed.
func ssCanonicalToolCalls(t *testing.T, value any) any {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		return value
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		call, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		name := call["name"]
		arguments := call["arguments"]
		if fn, ok := call["function"].(map[string]any); ok {
			if v, present := fn["name"]; present {
				name = v
			}
			if v, present := fn["arguments"]; present {
				arguments = v
			}
		}
		canonical := map[string]any{}
		if v, present := call["id"]; present {
			canonical["id"] = v
		}
		if name != nil {
			canonical["name"] = name
		}
		if arguments != nil {
			canonical["arguments"] = ssCanonicalArguments(arguments)
		}
		out = append(out, canonical)
	}
	return out
}

// canonicalArguments parses a string-encoded argument payload into its value.
func ssCanonicalArguments(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return text
	}
	return parsed
}

// jsonNumberToInt reads an integer that arrived as a JSON number.
func ssJSONInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	case float64:
		return int(n), nil
	case int:
		return n, nil
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

func ssJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func ssJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(raw)
}
