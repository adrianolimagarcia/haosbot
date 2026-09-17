package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/runtimecontext"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// Replay-history reconstruction.
//
// Mirrors Session.get_history (session/manager.py:344-475) and the three
// helpers it depends on: recent_message_start_index (utils/helpers.py:452),
// _sanitize_assistant_replay_text (session/manager.py:205) and
// content_with_media_breadcrumbs (utils/helpers.py:389).
//
// The message keys the reference reads literally are named here; the reference
// has no constants for any of them.

const (
	// CommandMeta is the "_command" key. A message carrying a truthy value is
	// a slash-command echo and never replayed (manager.py:382).
	CommandMeta = "_command"
	// MediaMeta is the "media" key holding persisted attachment paths.
	MediaMeta = "media"
	// CLIAppsMeta is the "cli_apps" key holding CLI-app attachments.
	CLIAppsMeta = "cli_apps"
	// cliAppsBreadcrumbLimit is the reference's `cli_apps[:8]` slice.
	cliAppsBreadcrumbLimit = 8
)

// historyView is the transcript as GetHistory sees it: the parsed messages plus
// the raw records they were loaded from.
//
// The raw record is needed because two things the reference reads off a message
// dict are not recoverable from core.Message:
//
//   - `key in message`, used both for the optional output keys and for the
//     empty-assistant skip rule. core.Message cannot report that a modelled key
//     was present with a null or empty value.
//   - `message.get("content", "")`, which distinguishes an ABSENT content key
//     (defaults to "") from a present null (stays None).
//
// A message this package created in memory has no record, and the view falls
// back to the parsed struct.
type historyView struct {
	messages []core.Message
	rawLines [][]byte
}

// recordFields returns the ordered fields of message i's stored record.
func (v historyView) recordFields(i int) ([]rawField, bool) {
	if i < 0 || i >= len(v.rawLines) || v.rawLines[i] == nil {
		return nil, false
	}
	fields, err := objectFields(v.rawLines[i])
	if err != nil {
		return nil, false
	}
	return fields, true
}

// extraAt returns the value stored under key for message i.
func (v historyView) extraAt(i int, key string) (json.RawMessage, bool) {
	if fields, ok := v.recordFields(i); ok {
		raw, present := fieldValue(fields, key)
		return raw, present
	}
	return v.messages[i].Extra(key)
}

// keyPresent reports Python's `key in message` for message i.
func (v historyView) keyPresent(i int, key string) bool {
	if fields, ok := v.recordFields(i); ok {
		_, present := fieldValue(fields, key)
		return present
	}
	return messageKeyPresent(v.messages[i], key)
}

// contentValue returns the value GetHistory starts from for message i:
// `message.get("content", "")`.
func (v historyView) contentValue(i int) any {
	if !v.keyPresent(i, "content") {
		return ""
	}
	m := &v.messages[i]
	if m.Content.IsZero() {
		return nil // the key is present and holds null
	}
	if m.Content.IsText() {
		return m.Content.Text
	}
	raw, err := json.Marshal(m.Content.Blocks)
	if err != nil {
		return nil
	}
	return decodeJSONValue(raw)
}

// messageKeyPresent is the fallback for messages with no stored record.
//
// It is exact for the keys this package can see. A modelled key that was
// present with a null or zero value is reported absent, because core.Message
// does not expose the presence bit for its own fields.
func messageKeyPresent(m core.Message, key string) bool {
	switch key {
	case "role", "content":
		return true // core.Message always marshals both
	case "tool_calls":
		return m.ToolCalls != nil
	case "tool_call_id":
		return m.ToolCallID != ""
	case "name":
		return m.Name != ""
	case "reasoning_content":
		return m.ReasoningContent != ""
	case "thinking_blocks":
		return m.ThinkingBlocks != nil
	}
	_, ok := m.Extra(key)
	return ok
}

// decodeJSONValue decodes a raw JSON value with UseNumber, matching how this
// package decodes every other JSON value it keeps in memory.
func decodeJSONValue(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil
	}
	return out
}

// pySliceStart reproduces Python's `seq[i:]` start index, including the
// negative-index behaviour that a negative last_archived can produce after
// commit_summary_checkpoint(insert_at=<negative>).
func pySliceStart(i, n int) int {
	if i < 0 {
		i += n
		if i < 0 {
			i = 0
		}
	}
	if i > n {
		i = n
	}
	return i
}

// GetHistory returns the recent replayable messages for LLM input.
//
// Mirrors Session.get_history (manager.py:344). maxTokens is the reference's
// keyword-only max_tokens and includeRuntimeContext is its
// include_runtime_context (default true).
//
// CAVEAT on maxTokens: the reference estimates tokens with tiktoken when it is
// importable (utils/helpers.py:813) and only falls back to a UTF-8 byte count
// otherwise; this port has no external dependencies and always uses
// agent.EstimateMessageTokens, the byte-count path. The SHAPE of the trimming
// (keep the newest messages, then re-anchor to the first user turn, then to a
// legal tool boundary) matches, but which messages fit does not, so token
// budgets are not differentially comparable.
func (s *Session) GetHistory(maxMessages, maxTokens int, extendToUser, includeRuntimeContext bool) []core.Message {
	s.mu.Lock()
	v := historyView{
		messages: append([]core.Message(nil), s.messages...),
		rawLines: make([][]byte, len(s.rawLines)),
	}
	copy(v.rawLines, s.rawLines)
	lastArchived := s.lastArchived
	s.mu.Unlock()

	lo := pySliceStart(lastArchived, len(v.messages))
	hi := len(v.messages)

	if maxMessages > 0 {
		lo += recentMessageStartIndex(v.messages[lo:hi], maxMessages, extendToUser)
	}

	// Avoid starting mid-turn when possible, except for proactive assistant
	// deliveries that the user may be replying to (manager.py:370-379).
	for i := lo; i < hi; i++ {
		if v.messages[i].Role != core.RoleUser {
			continue
		}
		if i > lo && viewMarkerTruthy(v, i-1, ChannelDeliveryMeta) {
			lo = i - 1
		} else {
			lo = i
		}
		break
	}

	// Drop orphan tool results at the front.
	if start := agent.FindLegalMessageStart(v.messages[lo:hi]); start > 0 {
		lo += start
	}

	out := make([]core.Message, 0, hi-lo)
	for i := lo; i < hi; i++ {
		if raw, ok := v.extraAt(i, CommandMeta); ok && pyTruthy(raw) {
			continue
		}

		markerRaw, hasMarker := v.extraAt(i, runtimecontext.HistoryMeta)
		hasPersistedRuntimeContext := hasMarker && isJSONObject(markerRaw)

		content := v.contentValue(i)
		if !includeRuntimeContext {
			cleaned := runtimecontext.PublicHistoryMessage(map[string]any{
				"content":                  content,
				runtimecontext.HistoryMeta: decodeJSONValue(markerRaw),
			})
			content = cleaned["content"]
		}

		role := string(v.messages[i].Role)
		if role == string(core.RoleAssistant) {
			if text, ok := content.(string); ok {
				content = sanitizeAssistantReplayText(text)
			}
		}

		media, _ := v.extraAt(i, MediaMeta)
		content = textutil.ContentWithMediaBreadcrumbs(role, content, decodeJSONValue(media))

		cliApps, _ := v.extraAt(i, CLIAppsMeta)
		if includeRuntimeContext && !hasPersistedRuntimeContext && role == string(core.RoleUser) {
			content = appendCLIAppBreadcrumbs(content, cliApps)
		}

		if role == string(core.RoleAssistant) {
			if text, ok := content.(string); ok && textutil.PyStrip(text) == "" {
				if !v.keyPresent(i, "tool_calls") &&
					!v.keyPresent(i, "reasoning_content") &&
					!v.keyPresent(i, "thinking_blocks") {
					continue
				}
			}
		}

		entry := core.Message{Role: v.messages[i].Role, Content: toCoreContent(content)}
		for _, key := range replayCopyKeys {
			if !v.keyPresent(i, key) {
				continue
			}
			switch key {
			case "tool_calls":
				entry.ToolCalls = v.messages[i].ToolCalls
			case "tool_call_id":
				entry.ToolCallID = v.messages[i].ToolCallID
			case "name":
				entry.Name = v.messages[i].Name
			case "reasoning_content":
				entry.ReasoningContent = v.messages[i].ReasoningContent
			case "thinking_blocks":
				entry.ThinkingBlocks = v.messages[i].ThinkingBlocks
			}
		}
		out = append(out, entry)
	}

	if maxTokens > 0 && len(out) > 0 {
		out = trimHistoryToTokens(out, maxTokens)
	}
	return out
}

// replayCopyKeys is the key list manager.py:437 iterates, in order.
var replayCopyKeys = []string{
	"tool_calls", "tool_call_id", "name", "reasoning_content", "thinking_blocks",
}

// trimHistoryToTokens mirrors the max_tokens block of get_history
// (manager.py:443-475).
func trimHistoryToTokens(out []core.Message, maxTokens int) []core.Message {
	kept := make([]core.Message, 0, len(out))
	used := 0
	for i := len(out) - 1; i >= 0; i-- {
		tokens := agent.EstimateMessageTokens(out[i])
		if len(kept) > 0 && used+tokens > maxTokens {
			break
		}
		kept = append(kept, out[i])
		used += tokens
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	// Keep history aligned to the first visible user turn.
	firstUser := -1
	for i := range kept {
		if kept[i].Role == core.RoleUser {
			firstUser = i
			break
		}
	}
	if firstUser >= 0 {
		kept = kept[firstUser:]
	} else {
		// Tight token budgets can otherwise leave assistant-only tails. If a
		// user turn exists in the unsliced output, recover the nearest one even
		// if it slightly exceeds the token budget.
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Role == core.RoleUser {
				kept = out[i:]
				break
			}
		}
	}

	// And keep a legal tool-call boundary at the front.
	if start := agent.FindLegalMessageStart(kept); start > 0 {
		kept = kept[start:]
	}
	return kept
}

// toCoreContent converts a Python content value into core.Content.
//
// A string and a list of block objects are modelled exactly. A null becomes the
// zero Content, which marshals back to null. Any other scalar (a number, a
// bool, a bare object) has no core.Content representation and becomes null;
// see the limitations note in the package report.
func toCoreContent(v any) core.Content {
	switch t := v.(type) {
	case nil:
		return core.Content{}
	case string:
		return core.TextContent(t)
	case []any:
		blocks := make([]core.ContentBlock, 0, len(t))
		for _, item := range t {
			raw, err := json.Marshal(item)
			if err != nil {
				return core.Content{}
			}
			var b core.ContentBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return core.Content{}
			}
			blocks = append(blocks, b)
		}
		return core.BlockContent(blocks)
	}
	return core.Content{}
}

// viewMarkerTruthy is markerTruthy against a historyView, so the check sees the
// stored record even for a message core.Message could not fully model.
func viewMarkerTruthy(v historyView, i int, key string) bool {
	raw, ok := v.extraAt(i, key)
	if !ok {
		return false
	}
	return pyTruthy(raw)
}

// recentMessageStartIndex ports recent_message_start_index
// (utils/helpers.py:452-476).
func recentMessageStartIndex(messages []core.Message, maxMessages int, extendToUser bool) int {
	if maxMessages <= 0 {
		return len(messages)
	}
	startIdx := len(messages) - maxMessages
	if startIdx < 0 {
		startIdx = 0
	}
	if !extendToUser || len(messages) <= maxMessages {
		return startIdx
	}
	for i := startIdx; i < len(messages); i++ {
		if messages[i].Role == core.RoleUser {
			return startIdx
		}
	}

	recoveredUser := -1
	for i := startIdx - 1; i >= 0; i-- {
		if messages[i].Role == core.RoleUser {
			recoveredUser = i
			break
		}
	}
	if recoveredUser < 0 {
		return startIdx
	}
	if recoveredUser > 0 && markerTruthy(messages[recoveredUser-1], ChannelDeliveryMeta) {
		return recoveredUser - 1
	}
	return recoveredUser
}

// appendCLIAppBreadcrumbs synthesizes the CLI-app attachment lines
// (manager.py:404-427).
func appendCLIAppBreadcrumbs(content any, cliApps json.RawMessage) any {
	text, isText := content.(string)
	if !isText {
		return content
	}
	items, err := arrayItems(cliApps)
	if err != nil || len(items) == 0 {
		return content
	}
	if len(items) > cliAppsBreadcrumbLimit {
		items = items[:cliAppsBreadcrumbLimit]
	}

	lines := make([]string, 0, len(items))
	for _, item := range items {
		fields, err := objectFields(item)
		if err != nil {
			continue
		}
		nameRaw, _ := fieldValue(fields, "name")
		name := strings.ToLower(textutil.PyStrip(pyStrOr(nameRaw, "")))
		if name == "" {
			continue
		}
		entryRaw, _ := fieldValue(fields, "entry_point")
		entryPoint := textutil.PyStrip(pyStrOr(entryRaw, "unknown"))
		if entryPoint == "" {
			entryPoint = "unknown"
		}
		lines = append(lines, "[CLI App Attachment: @"+name+"; tool=run_cli_app; entry_point="+
			entryPoint+"; skill=skills/cli-app-"+name+"/SKILL.md]")
	}
	if len(lines) == 0 {
		return content
	}
	breadcrumbs := strings.Join(lines, "\n")
	if text != "" {
		return text + "\n" + breadcrumbs
	}
	return breadcrumbs
}

// pyStrOr implements Python's `str(value or default)` for a raw JSON value.
func pyStrOr(raw json.RawMessage, def string) string {
	if len(bytes.TrimSpace(raw)) == 0 || !pyTruthy(raw) {
		return def
	}
	return pyStr(raw)
}

// ---------------------------------------------------------------------------
// Assistant replay sanitisation
// ---------------------------------------------------------------------------

// The three patterns _sanitize_assistant_replay_text applies
// (session/manager.py:36-38). They are matched by hand rather than with
// regexp, because two of them end in `\s*$` and Go's regexp `\s` is ASCII-only
// while Python's matches str.isspace() — the U+001C..U+001F gap that
// internal/textutil exists for.

var (
	// _MESSAGE_TIME_PREFIX_RE = ^\[Message Time: [^\]]+\]\n?
	messageTimePrefix = "[Message Time: "
	// _LOCAL_IMAGE_BREADCRUMB_RE = ^\[image: (?:/|~)[^\]]+\]\s*$
	localImageBreadcrumbPrefix = "[image: "
	// _TOOL_CALL_ECHO_RE = ^\s*(?:generate_image|message)\([^)]*\)\s*$
	toolCallEchoNames = [...]string{"generate_image", "message"}
)

// sanitizeAssistantReplayText ports _sanitize_assistant_replay_text
// (session/manager.py:205-219).
func sanitizeAssistantReplayText(content string) string {
	content = stripMessageTimePrefix(content)
	lines := pySplitLines(content)
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if matchLocalImageBreadcrumb(line) || matchToolCallEcho(line) {
			continue
		}
		kept = append(kept, line)
	}
	return textutil.PyStrip(strings.Join(kept, "\n"))
}

// stripMessageTimePrefix applies _MESSAGE_TIME_PREFIX_RE with count=1.
func stripMessageTimePrefix(s string) string {
	if !strings.HasPrefix(s, messageTimePrefix) {
		return s
	}
	rest := s[len(messageTimePrefix):]
	idx := strings.IndexByte(rest, ']')
	if idx <= 0 {
		// No closing bracket, or nothing between the prefix and it: `[^\]]+`
		// requires at least one character, so the pattern does not match.
		return s
	}
	end := len(messageTimePrefix) + idx + 1
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[end:]
}

// matchLocalImageBreadcrumb applies _LOCAL_IMAGE_BREADCRUMB_RE.
func matchLocalImageBreadcrumb(line string) bool {
	if !strings.HasPrefix(line, localImageBreadcrumbPrefix) {
		return false
	}
	body := line[len(localImageBreadcrumbPrefix):]
	if body == "" || (body[0] != '/' && body[0] != '~') {
		return false
	}
	body = body[1:]
	idx := strings.IndexByte(body, ']')
	if idx <= 0 {
		// `[^\]]+` needs at least one character and cannot span a ']', so the
		// first ']' is the only possible terminator.
		return false
	}
	return pyAllSpace(body[idx+1:])
}

// matchToolCallEcho applies _TOOL_CALL_ECHO_RE.
func matchToolCallEcho(line string) bool {
	i := 0
	for i < len(line) {
		r, size := utf8.DecodeRuneInString(line[i:])
		if !textutil.PyIsSpace(r) {
			break
		}
		i += size
	}
	rest := line[i:]
	body := ""
	found := false
	for _, name := range toolCallEchoNames {
		if strings.HasPrefix(rest, name+"(") {
			body = rest[len(name)+1:] // past the "(" as well
			found = true
			break
		}
	}
	if !found {
		return false
	}
	idx := strings.IndexByte(body, ')')
	if idx < 0 {
		return false
	}
	return pyAllSpace(body[idx+1:])
}

// pyAllSpace reports whether every rune is Python whitespace (an empty string
// qualifies, mirroring `\s*`).
func pyAllSpace(s string) bool {
	for _, r := range s {
		if !textutil.PyIsSpace(r) {
			return false
		}
	}
	return true
}

// pySplitLines ports str.splitlines() for the line boundaries Python
// recognises. Go's strings.Split on "\n" is not equivalent: Python also breaks
// on \r, \v, \f, \x1c, \x1d, \x1e, \x85, U+2028 and U+2029, treats \r\n as one
// boundary, and does not emit a trailing empty line.
func pySplitLines(s string) []string {
	out := make([]string, 0, strings.Count(s, "\n")+1)
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isPyLineBoundary(r) {
			i += size
			continue
		}
		out = append(out, s[start:i])
		i += size
		if r == '\r' && i < len(s) && s[i] == '\n' {
			i++
		}
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// isPyLineBoundary reports whether r ends a line for str.splitlines().
func isPyLineBoundary(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}
