package builtin

// Python value semantics for ApplyPatchTool's argument handling.
//
// ApplyPatchTool.execute (agent/tools/apply_patch.py:97-254) receives
// already-decoded Python objects and manipulates them with Python semantics:
// `if not edits`, `for edit_value in edits`, `isinstance(x, str)`, and the
// AttributeError/TypeError messages that escape when an argument has the wrong
// type. Those messages are model-visible text, so they are reproduced rather
// than replaced with a Go-flavoured error.
//
// The port's Registry does not run the reference's schema validation (it has no
// validate_params stage), so a wrongly-typed argument reaches the tool exactly
// as it would reach execute() when it is called directly.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// pyTypeName returns the Python type name of a decoded JSON value, as
// type(value).__name__ would report it. Numbers are classified the way
// json.loads does: an integer literal becomes an int, anything else a float.
func pyTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return "int"
		}
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return "object"
}

// pyTruthy mirrors bool(value) for the JSON values this tool can receive.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return true
		}
		return f != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// pyIterate mirrors `for x in value` for the JSON values this tool can receive.
// It reports false when the value is not iterable, which is the TypeError case.
//
// A str iterates its characters, a dict iterates its keys. Go map iteration
// order is unspecified, so dict keys are sorted: Python would use insertion
// order, but every key produces the same outcome here (none of them is a dict),
// so the observable result is identical and this stays deterministic.
func pyIterate(v any) ([]any, bool) {
	switch t := v.(type) {
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, true
	case []any:
		return t, true
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, k)
		}
		return out, true
	}
	return nil, false
}

// pyAttributeError builds the AttributeError message Python raises when a
// non-str argument reaches a str method, e.g.
// `'int' object has no attribute 'replace'`.
func pyAttributeError(attr string, v any) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", pyTypeName(v), attr)
}

// pyReprStr mirrors repr() for a str. _validate_patch_path embeds it in the
// null-byte error message, so `path!r` has to be reproduced.
//
// Python picks the quote that avoids escaping: ' unless the string contains a
// single quote and no double quote. Escapes are \n, \r, \t, \\, the quote
// itself, and \xHH / \uXXXX / \UXXXXXXXX for everything str.isprintable()
// rejects — which is the same predicate as Go's unicode.IsPrint.
func pyReprStr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
