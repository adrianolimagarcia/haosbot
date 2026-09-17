package skills

// Python value semantics for the small dynamic values frontmatter produces.
//
// parse_skill_metadata returns dict[str, object] whose values may be str, bool,
// int, float, None, list or dict, and the loader makes TRUTHINESS decisions on
// them (`meta.get("always")`, `value.strip()`, `isinstance(x, str)`). Go's zero
// values do not line up with Python's, so every such decision goes through the
// helpers here rather than through Go idioms.

import (
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// pyStr ports Python's str() for the value shapes frontmatter can produce.
// parse_skill_metadata stringifies mapping keys with str(key), so a numeric or
// boolean key becomes "1" / "True" rather than failing.
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
	case int:
		return itoa(int64(t))
	case int64:
		return itoa(t)
	case float64:
		return pyjson.FormatFloat(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, pyRepr(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return "<dict>"
	}
	return "<object>"
}

// pyRepr ports repr() for the list elements str() would render.
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
	}
	return pyStr(v)
}

// pyTruthy ports Python's truthiness for the value shapes frontmatter produces.
//
// The distinction from a nil check is load-bearing in two places:
// `meta.get("always") or meta.get("always")` (skills.py:357-358) and
// `_requirement_lists`'s `requires or {}` (skills.py:268). An empty string, an
// empty list, an empty dict, 0 and False are all FALSY in Python but non-nil in
// Go, so a `!= nil` test would invert the answer for e.g. `always: ""`.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// pyIsInstanceStr ports `isinstance(value, str)`.
func pyIsInstanceStr(v any) bool {
	_, ok := v.(string)
	return ok
}

// pyIsInstanceDict ports `isinstance(value, dict)`.
func pyIsInstanceDict(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// pyIsInstanceList ports `isinstance(value, list)`.
func pyIsInstanceList(v any) bool {
	_, ok := v.([]any)
	return ok
}

// pyStrippedTruthy ports the `isinstance(value, str) and value.strip()` test
// used by _requirement_lists: a string of only Python whitespace is falsy after
// strip(), and U+001C..U+001F count as whitespace to Python but not to
// strings.TrimSpace.
func pyStrippedTruthy(v any) bool {
	s, ok := v.(string)
	return ok && textutil.PyStrip(s) != ""
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [20]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// pyJSONString renders s the way json.dumps does with its default
// ensure_ascii=True, quotes included.
//
// It exists for _activation_marker (plugins.py:455-465), whose output is
// written to a marker file that the reference and the port both compare byte
// for byte. encoding/json cannot be used there: it escapes '<', '>' and '&' as
// \u003c/\u003e/\u0026, which Python leaves literal, so an encoded package path
// containing one of those characters would make the two implementations
// disagree about whether a plugin is enabled.
func pyJSONString(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 2)
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || r > 0x7e:
				writeJSONHexEscape(&sb, r)
			default:
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// writeJSONHexEscape writes \uXXXX, using a surrogate pair above the BMP as
// Python's json encoder does.
func writeJSONHexEscape(sb *strings.Builder, r rune) {
	const hexDigits = "0123456789abcdef"
	writeUnit := func(unit rune) {
		sb.WriteString(`\u`)
		sb.WriteByte(hexDigits[(unit>>12)&0xf])
		sb.WriteByte(hexDigits[(unit>>8)&0xf])
		sb.WriteByte(hexDigits[(unit>>4)&0xf])
		sb.WriteByte(hexDigits[unit&0xf])
	}
	if r > 0xffff {
		r -= 0x10000
		writeUnit(0xd800 + (r >> 10))
		writeUnit(0xdc00 + (r & 0x3ff))
		return
	}
	writeUnit(r)
}
