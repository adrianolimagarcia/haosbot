package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"github.com/adrianolimagarcia/nanobot-go/internal/runtimecontext"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// rawArchiveMaxChars is _RAW_ARCHIVE_MAX_CHARS (memory.py:750): the tighter cap
// a degraded (non-model) checkpoint is built with. append_history still applies
// its own emergency hard cap afterwards, with a different limit.
const rawArchiveMaxChars = 16000

// timestampChars is the `timestamp[:16]` slice in _format_messages.
const timestampChars = 16

// FormatMessages renders a transcript batch as the plain-text block that
// raw_archive persists (_format_messages, memory.py:642-663).
//
// Messages are map[string]any because the reference operates on the persisted
// message dicts, and a decoded JSON object is the only Go representation that
// can hold every value those dicts hold: `role`, `content`, `timestamp`,
// `media` and `tools_used` are all read as opaque values and stringified with
// Python's str(). core.Message cannot represent, for example, the integer
// timestamp the reference's own fixture uses (tests/agent/test_memory_store.py).
//
// DECODING CONTRACT: decode the messages with json.Decoder.UseNumber(). Python's
// json.loads keeps an integer an int, so str(1720000000) is "1720000000"; a
// plain json.Unmarshal into map[string]any yields float64, whose str() is
// "1720000000.0". The difference is visible in every timestamp of every line.
//
// Messages whose formatted content is falsy are skipped, so a batch can render
// to an empty string.
//
// DIVERGENCES, both confined to input shapes the reference never produces:
//
//   - A Python dict preserves insertion order and Go's map does not. When a
//     stringified value is a JSON object (only reachable through a multimodal
//     content list) its keys are rendered in sorted order here, where Python
//     would render them in document order. A single-key object renders
//     identically.
//   - Where Python raises TypeError (`, `.join over a list holding a non-string,
//     or over a non-iterable tools_used) this renders the element with str()
//     instead of failing. See pyJoin.
func FormatMessages(messages []map[string]any) string {
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		// Python's message.get("content", "") defaults only a MISSING key to
		// "", and "" is falsy exactly like a missing key, so the two cases
		// collapse into the same nil here.
		content := message["content"]
		role := message["role"]

		content = textutil.ContentWithMediaBreadcrumbs(pyStr(role), content, message["media"])
		if !pyTruthy(content) {
			continue
		}

		tools := ""
		if toolsUsed, ok := message["tools_used"]; ok && pyTruthy(toolsUsed) {
			tools = " [tools: " + pyJoin(", ", toolsUsed) + "]"
		}

		timestamp := "?"
		if raw, ok := message["timestamp"]; ok && raw != nil {
			timestamp = pyStr(raw)
		}

		roleText := pyStr(role)
		if !pyTruthy(role) {
			roleText = "unknown"
		}

		lines = append(lines, fmt.Sprintf("[%s] %s%s: %s",
			pyHead(timestamp, timestampChars), strings.ToUpper(roleText), tools, pyStr(content)))
	}
	return strings.Join(lines, "\n")
}

// RawArchive persists and returns a bounded raw checkpoint
// (raw_archive, memory.py:665-678).
//
// The checkpoint is built with the tighter raw cap and then appended through
// append_history, which applies its own (larger) emergency cap — the reference
// passes no max_chars to append_history, so neither does this.
func (s *MemoryStore) RawArchive(messages []map[string]any, maxChars *int, sessionKey string) (string, error) {
	checkpoint := s.BuildRawCheckpoint(messages, maxChars)
	if _, err := s.AppendHistory(checkpoint, nil, sessionKey); err != nil {
		return "", err
	}
	return checkpoint, nil
}

// BuildRawCheckpoint builds the same bounded checkpoint as RawArchive without
// writing it (_build_raw_checkpoint, memory.py:680-692).
//
// maxChars is a *int so that "not supplied" is distinguishable from 0: the
// reference uses `max_chars if max_chars is not None else _RAW_ARCHIVE_MAX_CHARS`,
// and an explicit 0 disables the cap instead of selecting the default.
func (s *MemoryStore) BuildRawCheckpoint(messages []map[string]any, maxChars *int) string {
	limit := rawArchiveMaxChars
	if maxChars != nil {
		limit = *maxChars
	}
	checkpoint := fmt.Sprintf("[RAW] %d messages\n%s",
		len(messages), FormatMessages(runtimecontext.PublicHistoryMessages(messages)))
	return s.normalizeHistoryEntry(checkpoint, &limit)
}

// ---------------------------------------------------------------------------
// Python value semantics
// ---------------------------------------------------------------------------

// pyStr renders a decoded JSON value the way Python's str() does.
//
// Only the types json.Unmarshal produces are handled; anything else falls back
// to fmt.Sprint, which no caller in the ported scope can reach.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return pyNumberStr(t)
	case float64:
		return pyFloatStr(t)
	case int:
		return strconv.Itoa(t)
	case []any:
		return pyReprList(t)
	case map[string]any:
		return pyReprMap(t)
	}
	return fmt.Sprint(v)
}

// pyRepr renders a decoded JSON value the way Python's repr() does.
//
// repr and str agree for every scalar; they differ only for strings (repr
// quotes them) and for containers, whose elements are repr'd.
func pyRepr(v any) string {
	switch t := v.(type) {
	case string:
		return pyReprString(t)
	case []any:
		return pyReprList(t)
	case map[string]any:
		return pyReprMap(t)
	}
	return pyStr(v)
}

func pyReprList(items []any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyRepr(item))
	}
	b.WriteByte(']')
	return b.String()
}

// pyReprMap renders a JSON object as Python's repr would.
//
// Keys are sorted: a Python dict preserves insertion order and a Go map does
// not, so document order is not recoverable here. A single-key object — the
// shape a content block's nested objects most often have — is unaffected.
func pyReprMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyReprString(k))
		b.WriteString(": ")
		b.WriteString(pyRepr(m[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// pyReprString ports CPython's str repr: single quotes unless the string
// contains one and no double quote, with the standard escapes.
//
// Characters outside ASCII are left literal when Go considers them printable,
// matching Python for every printable character. The printability tables are
// not identical (Go's unicode package and CPython's unicodedata can disagree
// on a handful of unassigned or format characters), so an exotic non-printable
// character can be escaped here where Python leaves it literal, or the reverse.
func pyReprString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}

	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r > 0x7e && !unicode.IsPrint(r):
				if r > 0xffff {
					fmt.Fprintf(&b, `\U%08x`, r)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyNumberStr ports str() for a JSON number.
//
// json.Number keeps the literal, so an integer literal is rendered verbatim
// (which is what Python's str(int) produces for a JSON integer, -0 excepted,
// where Python's int is 0). A literal with a fraction or an exponent is a
// Python float, whose str() is repr().
func pyNumberStr(n json.Number) string {
	literal := n.String()
	if !strings.ContainsAny(literal, ".eE") {
		if literal == "-0" {
			return "0"
		}
		return literal
	}
	f, err := n.Float64()
	if err != nil {
		return literal
	}
	return pyFloatStr(f)
}

// pyFloatStr ports str() for a float. It differs from pyjson.FormatFloat only
// for the non-finite values, which Python's str() spells in lower case
// ("inf"/"nan") where json.dumps spells them "Infinity"/"NaN". JSON cannot
// carry them, so this is unreachable from a decoded document.
func pyFloatStr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return pyjson.FormatFloat(f)
}

// pyTruthy ports Python's truthiness for decoded JSON values.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	}
	return true
}

// pyJoin ports Python's `sep.join(value)` for the shapes a decoded JSON
// document can hold.
//
// DIVERGENCE: Python raises TypeError for a list holding a non-string, and for
// a non-iterable value such as a number. Both are unreachable from the
// reference's own writer (tools_used is written as a list of tool names); this
// renders the offending value with str() instead of failing, so a malformed
// transcript degrades into a readable dump rather than aborting consolidation.
func pyJoin(sep string, v any) string {
	switch t := v.(type) {
	case string:
		// A Python str IS an iterable of characters.
		parts := make([]string, 0, len(t))
		for _, r := range t {
			parts = append(parts, string(r))
		}
		return strings.Join(parts, sep)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
				continue
			}
			parts = append(parts, pyStr(item))
		}
		return strings.Join(parts, sep)
	case map[string]any:
		// Python iterates a dict's keys, in insertion order; Go has none.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return strings.Join(keys, sep)
	}
	return pyStr(v)
}

// pyHead ports Python's `s[:n]` slice: CHARACTERS, and a shorter string is
// returned unchanged with no suffix appended (this is not truncate_text).
func pyHead(s string, n int) string {
	if n < 0 {
		n = 0
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
