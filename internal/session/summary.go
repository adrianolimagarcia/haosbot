package session

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// Summary checkpoint helpers.
//
// Mirrors nanobot/session/summary.py in full. A "summary checkpoint" is the
// durable boundary left behind when the agent replaces an old prefix of the
// transcript with a generated summary: a hidden user message carrying
// SummaryContinuationText, plus a "_last_summary" entry in the session
// metadata. Session.GetHistory resumes replay at that marker instead of
// replaying the transcript it replaced.

// SummaryContinuationText is SUMMARY_CONTINUATION_TEXT (summary.py:16-18).
const SummaryContinuationText = "Continue the active task from the working-memory checkpoint above."

// LastSummaryMetaKey is the metadata key holding the persisted summary. The
// reference spells it out literally in summary.py:44 and manager.py:336; it is
// named here because three call sites in this package share it.
const LastSummaryMetaKey = "_last_summary"

// SessionSummary mirrors the SessionSummary TypedDict (summary.py:26-28).
type SessionSummary struct {
	Text       string
	LastActive string
}

// SessionSummaryCheckpoint mirrors the frozen SessionSummaryCheckpoint
// dataclass (summary.py:31-36).
type SessionSummaryCheckpoint struct {
	Summary            string
	TranscriptBoundary int
}

// IsSummaryCheckpoint reports whether a persisted message is the durable
// boundary of a replacement summary.
//
// Mirrors is_summary_checkpoint (summary.py:20-26):
//
//	is_hidden_history_message(message) and message.get("content") == SUMMARY_CONTINUATION_TEXT
//
// The content comparison is a Python value comparison, so it is true only for
// the exact string; a missing key (None), a content list and a non-string
// scalar all compare unequal.
func IsSummaryCheckpoint(m core.Message) bool {
	if !IsHiddenHistoryMessage(m) {
		return false
	}
	return m.Content.IsText() && m.Content.Text == SummaryContinuationText
}

// SessionSummaryFromMetadata validates and normalises the persisted summary.
//
// Mirrors session_summary_from_metadata (summary.py:39-58):
//
//   - metadata may be nil (the reference's `metadata is not None` guard);
//   - "_last_summary" must be a Mapping, otherwise nil;
//   - "text" must be a non-empty str, otherwise nil;
//   - "last_active" is kept verbatim when datetime.fromisoformat accepts it,
//     and replaced by fallbackLastActive.isoformat() otherwise — including when
//     it is absent or is not a str.
//
// fallbackLastActive is a naive local datetime in every reference call site
// (session.updated_at, agent/memory.py:1256), so it is rendered with
// formatNaive; the reference's isoformat() would add an offset for an aware
// datetime, which no caller ever supplies.
func SessionSummaryFromMetadata(metadata map[string]any, fallbackLastActive time.Time) *SessionSummary {
	var raw any
	if metadata != nil {
		raw = metadata[LastSummaryMetaKey]
	}
	summaryData, ok := raw.(map[string]any)
	if !ok {
		return nil
	}

	text, ok := summaryData["text"].(string)
	if !ok || text == "" {
		return nil
	}

	lastActive := formatNaive(fallbackLastActive)
	if rawLastActive, ok := summaryData["last_active"].(string); ok {
		if fromisoformatOK(rawLastActive) {
			lastActive = rawLastActive
		}
	}
	return &SessionSummary{Text: text, LastActive: lastActive}
}

// CommitSummaryCheckpoint replaces replay before a hidden boundary while
// preserving the transcript.
//
// Mirrors Session.commit_summary_checkpoint (manager.py:323-342):
//
//   - the marker is inserted as a USER message carrying
//     HiddenHistoryMeta: true and the current local timestamp;
//   - metadata["_last_summary"] becomes {"text": summary, "last_active": ...};
//   - last_archived is set to the RAW boundary, not the clamped one.
//
// insertAt is the reference's keyword-only insert_at (nil means "append at the
// end"); lastActive is its last_active (nil means "use updated_at", which is
// the reference's `last_active or self.updated_at`).
//
// Two details are reproduced deliberately:
//
//   - The marker's on-disk key order is role, content, _hidden_history,
//     timestamp. core.Message marshals its unknown keys AFTER timestamp, so the
//     exact record is built here and handed to the store as the verbatim line,
//     which is the same mechanism the loader uses for records read from disk.
//   - The insert index follows Python's list.insert clamping while
//     last_archived keeps the unclamped value. For an out-of-range insert_at
//     the two therefore disagree, and the disagreement is persisted.
func (s *Session) CommitSummaryCheckpoint(summary string, insertAt *int, lastActive *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	boundary := len(s.messages)
	if insertAt != nil {
		boundary = *insertAt
	}

	now := nowTimestamp()
	timestamp := formatNaive(now)
	marker := core.Message{
		Role:      core.RoleUser,
		Content:   core.TextContent(SummaryContinuationText),
		Timestamp: timestamp,
	}
	marker.SetExtra(HiddenHistoryMeta, json.RawMessage("true"))

	// The reference writes `(last_active or self.updated_at).isoformat()`, so a
	// nil last_active means "this session's updated_at" — NOT the current time.
	// Using the wall clock here made the checkpoint metadata depend on when the
	// call ran instead of on the session, which the differential harness caught:
	// the reference reported the updated_at loaded from the file while this port
	// reported a live timestamp.
	//
	// Neither insertMessageLocked nor setMetaRawLocked touches updatedAt, which
	// is what makes reading it here equivalent to the reference reading
	// self.updated_at after its raw list insert.
	active := s.updatedAt
	if lastActive != nil {
		active = *lastActive
	}
	lastActiveText := formatNaive(active)

	s.insertMessageLocked(boundary, marker, summaryMarkerRecord(timestamp))
	s.setMetaRawLocked(LastSummaryMetaKey, map[string]any{
		"text":        summary,
		"last_active": lastActiveText,
	}, lastSummaryRecord(summary, lastActiveText))
	s.lastArchived = boundary
}

// summaryMarkerRecord renders the continuation marker exactly as Python's
// json.dumps would (manager.py:334-339), key order included.
func summaryMarkerRecord(timestamp string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	writePyString(&buf, "role")
	buf.WriteString(pyKeySep)
	writePyString(&buf, string(core.RoleUser))
	buf.WriteString(pyItemSep)
	writePyString(&buf, "content")
	buf.WriteString(pyKeySep)
	writePyString(&buf, SummaryContinuationText)
	buf.WriteString(pyItemSep)
	writePyString(&buf, HiddenHistoryMeta)
	buf.WriteString(pyKeySep)
	buf.WriteString("true")
	buf.WriteString(pyItemSep)
	writePyString(&buf, "timestamp")
	buf.WriteString(pyKeySep)
	writePyString(&buf, timestamp)
	buf.WriteByte('}')
	return buf.Bytes()
}

// lastSummaryRecord renders the "_last_summary" value exactly as Python's
// json.dumps would (manager.py:336-339): {"text": ..., "last_active": ...}.
func lastSummaryRecord(text, lastActive string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	writePyString(&buf, "text")
	buf.WriteString(pyKeySep)
	writePyString(&buf, text)
	buf.WriteString(pyItemSep)
	writePyString(&buf, "last_active")
	buf.WriteString(pyKeySep)
	writePyString(&buf, lastActive)
	buf.WriteByte('}')
	return buf.Bytes()
}
